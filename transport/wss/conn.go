package wss

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// streamConn adapts binary WebSocket messages to net.Conn byte-stream
// semantics. One reader and one writer may run concurrently; additional reads
// and writes are serialized as required by gorilla/websocket.
type streamConn struct {
	conn *websocket.Conn

	readMu sync.Mutex
	reader io.Reader

	writeMu sync.Mutex
	close   sync.Once
}

func newStreamConn(conn *websocket.Conn) net.Conn {
	return &streamConn{conn: conn}
}

func (c *streamConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		if c.reader == nil {
			messageType, reader, err := c.conn.NextReader()
			if err != nil {
				return 0, normalizeWebSocketReadError(err)
			}
			if messageType != websocket.BinaryMessage {
				return 0, errUnexpectedMessageType
			}
			c.reader = reader
		}

		n, err := c.reader.Read(p)
		if errors.Is(err, io.EOF) {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *streamConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	written := 0
	for written < len(p) {
		end := written + maxFramePayload
		if end > len(p) {
			end = len(p)
		}
		if err := c.conn.WriteMessage(websocket.BinaryMessage, p[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

func (c *streamConn) Close() error {
	var err error
	c.close.Do(func() {
		err = c.conn.Close()
	})
	return err
}

func (c *streamConn) LocalAddr() net.Addr {
	return c.conn.UnderlyingConn().LocalAddr()
}

func (c *streamConn) RemoteAddr() net.Addr {
	return c.conn.UnderlyingConn().RemoteAddr()
}

func (c *streamConn) SetDeadline(deadline time.Time) error {
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	return c.conn.SetWriteDeadline(deadline)
}

func (c *streamConn) SetReadDeadline(deadline time.Time) error {
	return c.conn.SetReadDeadline(deadline)
}

func (c *streamConn) SetWriteDeadline(deadline time.Time) error {
	return c.conn.SetWriteDeadline(deadline)
}

func normalizeWebSocketReadError(err error) error {
	if errors.Is(err, net.ErrClosed) || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return io.EOF
	}
	return err
}
