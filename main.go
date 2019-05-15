package main

import (
    "./gopool"

    "flag"
    "log"
    "net"
    "time"

    "github.com/gobwas/ws"
    "github.com/mailru/easygo/netpoll"
)

var (
    addr          = flag.String("listen", ":8080", "address to bind to")
    max_conn_num  = flag.Int("max_conn_num", 20000, "max listen connection")
    max_read_size = flag.Int64("max_read_size", 32*1024, "max read size")
    server        = flag.String("server", "127.0.0.1:8000", "backend server address")
    workers       = flag.Int("workers", 128, "max workers count")
    queue         = flag.Int("queue", 1, "workers task queue size")
    ioTimeout     = flag.Duration("io_timeout", time.Millisecond*1000, "i/o operations timeout")
)

func main() {
    flag.Parse()

    // Initialize netpoll instance. We will use it to be noticed about incoming
    // events from listener of user connections.
    poller, err := netpoll.New(nil)
    if err != nil {
        log.Fatal(err)
    }

    var (
        // Make pool of X size, Y sized work queue and one pre-spawned
        // goroutine.
        pool = gopool.NewPool(*workers, *queue, 1)
        exit = make(chan struct{})
    )
    close_all := func(bridge *Bridge) {
        poller.Stop(bridge.cli_desc)
        poller.Stop(bridge.svr_desc)
        bridge.Close()
    }

    // handle is a new incoming connection handler.
    // It upgrades TCP connection to WebSocket, registers netpoll listener on
    //
    // We will call it below within accept() loop.
    handle := func(conn net.Conn) {
        // NOTE: we wrap conn here to show that ws could work with any kind of
        // io.ReadWriter.
        safeConn := deadliner{conn, *ioTimeout, nameConn(conn, false)}

        // Zero-copy upgrade to WebSocket connection.
        _, err := ws.Upgrade(safeConn)
        if err != nil {
            log.Printf("%s: upgrade error: %v", safeConn.name, err)
            conn.Close()
            return
        }

        // log.Printf("%s: established websocket connection", safeConn.name)

        // Register incoming user in chat.
        bridge, err := create_bridge(safeConn, *server)
        if err != nil {
            log.Printf("%s: dail server error: %v", safeConn.name, err)
            conn.Close()
            return
        }

        // Create netpoll event descriptor for conn.
        // We want to handle only read events of it.
        client_desc := netpoll.Must(netpoll.HandleRead(conn))
        server_desc := netpoll.Must(netpoll.HandleRead(bridge.server_conn))
        bridge.Add_Desc(client_desc, server_desc)

        // Subscribe to events about client conn.
        poller.Start(client_desc, func(ev netpoll.Event) {
            if ev&(netpoll.EventReadHup|netpoll.EventHup) != 0 {
                // When ReadHup or Hup received, this mean that client has
                // closed at least write end of the connection or connections
                // itself. So we want to stop receive events about such conn
                // and disconnect from the server.
                close_all(bridge)
                return
            }
            // Here we can read some new message from connection.
            // We can not read it right here in callback, because then we will
            // block the poller's inner loop.
            // We do not want to spawn a new goroutine to read single message.
            // But we want to reuse previously spawned goroutine.
            pool.Schedule(func() {
                if err := bridge.Forward(); err != nil {
                    // When receive failed, we can only disconnect broken
                    // connection and stop to receive events about it.
                    close_all(bridge)
                }
            })
        })
        // netpoll.Must(netpoll.HandleRead(bridge.server_conn))
        // bridge.server_conn.Close()
        // Subscribe to events about server conn.
        poller.Start(server_desc, func(ev netpoll.Event) {
            if ev&(netpoll.EventReadHup|netpoll.EventHup) != 0 {
                // When ReadHup or Hup received, this mean that server has
                // closed at least write end of the connection or connections
                // itself. So we want to stop receive events about such conn
                // and disconnect from the client.
                close_all(bridge)
                return
            }
            // Here we can read some new message from connection.
            // We can not read it right here in callback, because then we will
            // block the poller's inner loop.
            // We do not want to spawn a new goroutine to read single message.
            // But we want to reuse previously spawned goroutine.
            pool.Schedule(func() {
                if err := bridge.Response(); err != nil {
                    // When receive failed, we can only disconnect broken
                    // connection and stop to receive events about it.
                    close_all(bridge)
                }
            })
        })
    }

    // Create incoming connections listener.
    ln, err := net.Listen("tcp", *addr)
    if err != nil {
        log.Fatal(err)
    }
    ln = LimitListener(ln, *max_conn_num)

    log.Printf("websocket is listening on %s", ln.Addr().String())
    defer ln.Close()

    // Create netpoll descriptor for the listener.
    // We use OneShot here to manually resume events stream when we want to.
    acceptDesc := netpoll.Must(netpoll.HandleListener(
        ln, netpoll.EventRead|netpoll.EventOneShot,
    ))

    // accept is a channel to signal about next incoming connection Accept()
    // results.
    accept := make(chan error, 1)

    // Subscribe to events about listener.
    poller.Start(acceptDesc, func(e netpoll.Event) {
        // We do not want to accept incoming connection when goroutine pool is
        // busy. So if there are no free goroutines during 1ms we want to
        // cooldown the server and do not receive connection for some short
        // time.
        err := pool.ScheduleTimeout(time.Millisecond, func() {
            conn, err := ln.Accept()
            if err != nil {
                accept <- err
                return
            }

            accept <- nil
            handle(conn)
        })
        if err == nil {
            err = <-accept
        }
        if err != nil {
            if err != gopool.ErrScheduleTimeout {
                goto cooldown
            }
            if ne, ok := err.(net.Error); ok && ne.Temporary() {
                goto cooldown
            }

            log.Fatalf("accept error: %v", err)

        cooldown:
            delay := 5 * time.Millisecond
            log.Printf("accept error: %v; retrying in %s", err, delay)
            time.Sleep(delay)
        }

        poller.Resume(acceptDesc)
    })

    <-exit
}
