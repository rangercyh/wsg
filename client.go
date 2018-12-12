package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type Client struct {
	// The websocket connection from client
	clientConn *websocket.Conn

	// connection to server
	serverConn *ServerConn
}

// readPump pumps messages from the websocket connection to the server.
func (c *Client) readPump() {
	defer func() {
		c.clientConn.Close()
		c.serverConn.Close()
	}()
	c.clientConn.SetReadLimit(maxMessageSize)
	for {
		_, message, err := c.clientConn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}

		if _, err = c.serverConn.Write(message); err != nil {
			log.Printf("error: %v", err)
			break
		}
	}
}

// writePump pumps messages from the server to the websocket connection.
func (c *Client) writePump() {
	defer func() {
		c.serverConn.Close()
		c.clientConn.Close()
	}()

	scanner := bufio.NewScanner(c.serverConn)
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
		c.clientConn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := c.clientConn.WriteMessage(websocket.TextMessage, scanner.Bytes()); err != nil {
			log.Printf("error: %v", err)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		c.clientConn.WriteMessage(websocket.CloseMessage, []byte{})
		log.Printf("error: %v", err)
	}
}

func (c *Client) run() {
	go c.readPump()
	go c.writePump()
}

// serveWs handles websocket requests from the peer.
func serveWs(server *Server, w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	serverConn, err := server.Dial()
	if err != nil {
		clientConn.Close()
		log.Println(err)
		return
	}
	client := &Client{clientConn: clientConn, serverConn: serverConn}
	client.run()
}
