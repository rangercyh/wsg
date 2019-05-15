package main

import (
    "bufio"
    "bytes"
    "encoding/binary"
    "github.com/gobwas/ws"
    "github.com/gobwas/ws/wsutil"
    "github.com/mailru/easygo/netpoll"
    "io"
    "log"
    "net"
    // "reflect"
    "sync"
    "time"
)

func nameConn(conn net.Conn, active bool) string {
    if active {
        return conn.LocalAddr().String() + " [tcp>tcp] " + conn.RemoteAddr().String()
    } else {
        return conn.RemoteAddr().String() + " [ws>tcp] " + conn.LocalAddr().String()
    }
}

// deadliner is a wrapper around net.Conn that sets read/write deadlines before
// every Read() or Write() call.
type deadliner struct {
    net.Conn
    t    time.Duration
    name string
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

func (d deadliner) Close() error {
    return d.Conn.Close()
}

type Bridge struct {
    io          sync.Mutex
    client_conn io.ReadWriteCloser
    client_name string
    server_conn net.Conn
    server_name string

    cli_desc *netpoll.Desc
    svr_desc *netpoll.Desc
}

func (b *Bridge) Forward() error {
    b.io.Lock()
    defer b.io.Unlock()

    h, r, err := wsutil.NextReader(b.client_conn, ws.StateServerSide)
    if err != nil {
        log.Printf("%s client conn read error: %v", b.client_name, err)
        return err
    }
    if h.OpCode.IsControl() {
        err = wsutil.ControlFrameHandler(b.client_conn, ws.StateServerSide)(h, r)
        if err != nil {
            log.Printf("%s client conn control process error: %v", b.client_name, err)
        }
        return err
    }
    lr := LimitReader(r, *max_read_size)
    if _, err = io.Copy(b.server_conn, lr); err != nil {
        log.Printf("%s forward copy error: %v", b.client_name, err)
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
        if err := wsutil.WriteServerMessage(b.client_conn, ws.OpText, scanner.Bytes()); err != nil {
            log.Printf("%s response to client error: %v", b.server_name, err)
        }
    }
    if err := scanner.Err(); err != nil {
        log.Printf("%s server conn scanner error: %v", b.server_name, err)
        return err
    }

    return nil
}

func (b *Bridge) Close() {
    b.client_conn.Close()
    b.server_conn.Close()
    b.cli_desc.Close()
    b.svr_desc.Close()
}

func (b *Bridge) Add_Desc(cli_desc *netpoll.Desc, svr_desc *netpoll.Desc) {
    b.cli_desc = cli_desc
    b.svr_desc = svr_desc
}

func create_bridge(cli_deal deadliner, server_addr string) (*Bridge, error) {
    svr_conn, err := net.Dial("tcp", server_addr)
    if err != nil {
        return nil, err
    }

    bridge := &Bridge{
        client_conn: cli_deal,
        client_name: cli_deal.name,
        server_conn: svr_conn,
        server_name: nameConn(svr_conn, true),
    }

    // log.Printf("%s create tcp socket to game", bridge.server_name)

    return bridge, nil
}
