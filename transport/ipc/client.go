package ipc

import (
	"net"
	"sync"
	"time"
)

// Client connects to an IPC Server.
type Client struct {
	conn    net.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	cb      func(MsgType, []byte)
	closed  bool
}

// Dial connects to path and starts a read loop.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn}
	go c.readLoop()
	return c, nil
}

// SetHandler installs the message callback. Safe to call before or after
// Dial; messages already delivered are not replayed.
func (c *Client) SetHandler(cb func(MsgType, []byte)) {
	c.mu.Lock()
	c.cb = cb
	c.mu.Unlock()
}

func (c *Client) readLoop() {
	for {
		typ, payload, err := ReadFrame(c.conn)
		if err != nil {
			c.mu.Lock()
			c.closed = true
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		cb := c.cb
		c.mu.Unlock()
		if cb != nil {
			cb(typ, payload)
		}
	}
}

// Send writes a frame to the server.
func (c *Client) Send(typ MsgType, payload interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return WriteFrame(c.conn, typ, payload)
}

func (c *Client) SendCookiesOffer(p *CookiesOfferPayload) error {
	return c.Send(MsgCookiesOffer, p)
}

func (c *Client) SendCommand(p *CommandPayload) error {
	return c.Send(MsgCommand, p)
}

// Close closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.conn.Close()
}
