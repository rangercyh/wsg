package main

import (
    "bufio"
    "bytes"
    "encoding/binary"
    "flag"
    "gopool"
    "io"
    "log"
    "net"
    "time"

    "github.com/gobwas/ws"
    "github.com/gobwas/ws/wsutil"
    "github.com/mailru/easygo/netpoll"

    "net/http"
)

var (
    addr      = flag.String("listen", ":8080", "address to bind to")
    server    = flag.String("server", "127.0.0.1:8000", "backend server address")
    workers   = flag.Int("workers", 128, "max workers count")
    queue     = flag.Int("queue", 1, "workers task queue size")
    ioTimeout = flag.Duration("io_timeout", time.Millisecond*1000, "i/o operations timeout")
)

func nameConn(conn net.Conn) string {
    return conn.LocalAddr().String() + " > " + conn.RemoteAddr().String()
}

// deadliner is a wrapper around net.Conn that sets read/write deadlines before
// every Read() or Write() call.
type deadliner struct {
    net.Conn
    t time.Duration
}

func (d deadliner) Write(p []byte) (int, error) {
    if err := d.Conn.SetWriteDeadline(time.Now().Add(d.t)); err != nil {
        return 0, err
    }
    return d.Conn.Write(p)
}

func (d deadliner) Read(p []byte) (int, error) {
    if err := d.Conn.SetReadDeadline(time.Now().Add(d.t)); err != nil {
        return 0, err
    }
    return d.Conn.Read(p)
}

type Bridge struct {
    io          sync.Mutex
    client_conn io.ReadWriteCloser
    server_conn io.ReadWriteCloser
    pool        *gopool.Pool
}

func (b *Bridge) Forward() error {
    b.io.Lock()
    defer b.io.Unlock()

    h, r, err := wsutil.NextReader(b.client_conn, ws.StateServerSide)
    if err != nil {
        log.Printf("%s client conn read error: %v", nameConn(b.client_conn), err)
        return err
    }
    if h.OpCode.IsControl() {
        err = wsutil.ControlFrameHandler(b.client_conn, ws.StateServerSide)(h, r)
        if err != nil {
            log.Printf("%s client conn control process error: %v", nameConn(b.client_conn), err)
        }
        return err
    }
    if _, err = io.Copy(b.server_conn, r); err != nil {
        log.Printf("%s forward copy error: %v", nameConn(b.client_conn), err)
        return err
    }

    return nil
}

func (b *Bridge) Response() error {
    b.io.Lock()
    defer b.io.Unlock()

    scanner := bufio.NewScanner(b.server_conn)
    split := func(data []byte, atEOF bool) (advance int, token []byte, err error) {
        if !atEOF && len(data) > 2 {
            var dataLen int16
            binary.Read(bytes.NewReader(data[0:2]), binary.BigEndian, &dataLen)
            if len(data) >= int(dataLen)+2 {
                return int(dataLen) + 2, data[2 : int(dataLen)+2], nil
            }
        }
        return
    }
    scanner.Split(split)
    for scanner.Scan() {
        if err := wsutil.WriteServerMessage(b.client_conn, ws.OpText, scanner.byte()); err != nil {
            log.Printf("%s response to client error: %v", nameConn(b.server_conn), err)
        }
    }
    if err := scanner.Err(); err != nil {
        log.Printf("%s server conn scanner error: %v", nameConn(b.server_conn), err)
        return err
    }

    return nil
}

func (b *Bridge) Close() {
    b.client_conn.Close()
    b.server_conn.Close()
}

func create_bridge(pool *gopool.Pool, conn net.Conn, server_addr string) *Bridge {
    svr_conn, err := net.Dial("tcp", server_addr)
    if err != nil {
        return nil, err
    }

    bridge := &Bridge{
        client_conn: conn,
        server_conn: svr_conn,
        pool:        pool,
    }

    return bridge
}

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
    // handle is a new incoming connection handler.
    // It upgrades TCP connection to WebSocket, registers netpoll listener on
    //
    // We will call it below within accept() loop.
    handle := func(conn net.Conn) {
        // NOTE: we wrap conn here to show that ws could work with any kind of
        // io.ReadWriter.
        safeConn := deadliner{conn, *ioTimeout}

        // Zero-copy upgrade to WebSocket connection.
        hs, err := ws.Upgrade(safeConn)
        if err != nil {
            log.Printf("%s: upgrade error: %v", nameConn(conn), err)
            conn.Close()
            return
        }

        log.Printf("%s: established websocket connection: %+v", nameConn(conn), hs)

        // Register incoming user in chat.
        bridge, err := create_bridge(pool, safeConn, *server)
        if err != nil {
            log.Printf("%s: dail server error: %v", nameConn(conn), err)
            conn.Close()
            return
        }

        // Create netpoll event descriptor for conn.
        // We want to handle only read events of it.
        client_desc := netpoll.Must(netpoll.HandleRead(bridge.client_conn))

        // Subscribe to events about client conn.
        poller.Start(client_desc, func(ev netpoll.Event) {
            if ev&(netpoll.EventReadHup|netpoll.EventHup) != 0 {
                // When ReadHup or Hup received, this mean that client has
                // closed at least write end of the connection or connections
                // itself. So we want to stop receive events about such conn
                // and disconnect from the server.
                poller.Stop(client_desc)
                bridge.close()
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
                    poller.Stop(client_desc)
                    bridge.close()
                }
            })
        })

        server_desc := netpoll.Must(netpoll.HandleRead(bridge.server_conn))
        // Subscribe to events about server conn.
        poller.Start(server_desc, func(ev netpoll.Event) {
            if ev&(netpoll.EventReadHup|netpoll.EventHup) != 0 {
                // When ReadHup or Hup received, this mean that server has
                // closed at least write end of the connection or connections
                // itself. So we want to stop receive events about such conn
                // and disconnect from the client.
                poller.Stop(server_desc)
                bridge.close()
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
                    poller.Stop(server_desc)
                    bridge.close()
                }
            })
        })
    }

    // Create incoming connections listener.
    ln, err := net.Listen("tcp", *addr)
    if err != nil {
        log.Fatal(err)
    }

    log.Printf("websocket is listening on %s", ln.Addr().String())

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
