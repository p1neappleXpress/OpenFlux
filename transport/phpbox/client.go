package phpbox

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client is a Mux over the direct-HTTP two-channel carrier - for a phpbox
// exit reachable over plain HTTP (no OpenFlux carrier in between). For the
// censored case the mux rides a real transport (e.g. cupsonline) instead;
// see NewMux. The public surface (Dial, Close) comes from the embedded Mux.
type Client struct {
	*Mux
}

// NewClient starts a mux against endpoint (the URL of phpbox.php) over the
// direct-HTTP carrier: a long-poll down GET and short up POSTs.
func NewClient(endpoint, token, session string) *Client {
	hc := newHTTPCarrier(endpoint, token, session)
	m := NewMux(hc)
	hc.onClosed = m.markClosed // the down-poll ending is a dead session
	_ = m.Start()
	return &Client{Mux: m}
}

// httpCarrier speaks the two-channel HTTP link phpbox.php exposes: it streams
// exit->client bytes from a long-poll GET (down) and batches client->exit
// bytes into short POSTs (up), because such hosts buffer request bodies but
// stream responses.
type httpCarrier struct {
	endpoint, token, session string
	http                     *http.Client

	ctx      context.Context
	cancel   context.CancelFunc
	upCh     chan []byte
	recv     func([]byte)
	onClosed func()
}

func newHTTPCarrier(endpoint, token, session string) *httpCarrier {
	ctx, cancel := context.WithCancel(context.Background())
	return &httpCarrier{
		endpoint: endpoint,
		token:    token,
		session:  session,
		http:     &http.Client{Timeout: 0}, // the down-poll is long-lived
		ctx:      ctx,
		cancel:   cancel,
		upCh:     make(chan []byte, 256),
	}
}

func (h *httpCarrier) Receive(cb func([]byte)) { h.recv = cb }

func (h *httpCarrier) Start() error {
	go h.downLoop()
	go h.upLoop()
	return nil
}

func (h *httpCarrier) Stop() error {
	h.cancel()
	return nil
}

func (h *httpCarrier) Send(p []byte) error {
	select {
	case h.upCh <- append([]byte(nil), p...):
	case <-h.ctx.Done():
	}
	return nil
}

func (h *httpCarrier) url(role string) string {
	v := url.Values{"k": {h.token}, "s": {h.session}, "r": {role}}
	return h.endpoint + "?" + v.Encode()
}

// upLoop batches queued bytes and POSTs them to the up route.
func (h *httpCarrier) upLoop() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var buf []byte
	flush := func() {
		if len(buf) == 0 {
			return
		}
		req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.url("up"), bytes.NewReader(buf))
		if err == nil {
			req.Header.Set("Content-Type", "application/octet-stream")
			if resp, err := h.http.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
		buf = buf[:0]
	}
	for {
		select {
		case p := <-h.upCh:
			buf = append(buf, p...)
			for drained := false; !drained; {
				select {
				case p2 := <-h.upCh:
					buf = append(buf, p2...)
				default:
					drained = true
				}
			}
		case <-t.C:
			flush()
		case <-h.ctx.Done():
			flush()
			return
		}
	}
}

// downLoop holds the streaming GET and feeds exit->client bytes to the mux
// until the exit caps it or it errors, then reports the session closed.
func (h *httpCarrier) downLoop() {
	if h.onClosed != nil {
		defer h.onClosed()
	}
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, h.url("down"), nil)
	if err != nil {
		return
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	tmp := make([]byte, 32768)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 && h.recv != nil {
			h.recv(tmp[:n])
		}
		if rerr != nil {
			return
		}
	}
}
