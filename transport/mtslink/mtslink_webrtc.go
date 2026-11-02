// Package mtslink implements WebRTC-based transport through MTS-Link meetings.
// Joins an MTS-Link video conference as a guest and tunnels data through
// WebRTC DataChannels.
package mtslink

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"

	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"golang.org/x/net/publicsuffix"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

const (
	webrtcUserAgent  = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	webrtcAppVersion = "3.9.207"
)

// MTSLinkWebRTCTransport implements Transport over MTS-Link WebRTC
type MTSLinkWebRTCTransport struct {
	*transport.BaseTransport

	meetingURL string
	userName   string
	isExit     bool // true for exit node, false for client

	// HTTP client with cookies
	httpClient *http.Client
	deviceID   string

	// Session state
	eventID         string
	sessionID       string
	userId          string
	participationID string
	joinToken       string
	conferenceID    string

	// WebRTC
	peerConnection *webrtc.PeerConnection
	dataChannel    *webrtc.DataChannel
	publishToken   string

	// WebSocket for signaling
	wsConn *websocket.Conn
	wsMu   sync.Mutex

	// Data handling
	onDataMu sync.RWMutex
	onData   func([]byte)

	sendQueue chan []byte

	closeOnce sync.Once
	done      chan struct{}
}

// NewMTSLinkWebRTCTransport creates WebRTC-based MTS-Link transport
func NewMTSLinkWebRTCTransport(meetingURL string, config transport.TransportConfig) *MTSLinkWebRTCTransport {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})

	return &MTSLinkWebRTCTransport{
		BaseTransport: transport.NewBaseTransport(config),
		meetingURL:    meetingURL,
		userName:      "OpenFluxBot",
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
		deviceID:  generateDeviceID(),
		sendQueue: make(chan []byte, config.MaxQueueSize),
		done:      make(chan struct{}),
	}
}

// Helper structures for API responses
type EventResponse struct {
	ID            int `json:"id"`
	EventSessions []struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	} `json:"eventSessions"`
}

type GuestLoginResponse struct {
	User struct {
		SessionID string `json:"sessionID"`
	} `json:"user"`
	Participation struct {
		User struct {
			ID int `json:"id"`
		} `json:"user"`
	} `json:"participation"`
}

type ConferenceResponse struct {
	ID         int    `json:"id"`
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
	UserID     int    `json:"userId"`
}

type JoinTokenResponse struct {
	Data struct {
		JoinToken string `json:"joinToken"`
	} `json:"data"`
}

// Start implements Transport.Start
func (t *MTSLinkWebRTCTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	utils.Debugf("[MTS-WebRTC] Starting WebRTC transport to %s", t.meetingURL)

	t.closeOnce = sync.Once{}
	t.done = make(chan struct{})

	utils.SafeGo("mts-webrtc.connect", func() { t.connectLoop() })

	return nil
}

// Stop implements Transport.Stop
func (t *MTSLinkWebRTCTransport) Stop() error {
	t.closeOnce.Do(func() {
		close(t.done)
		if t.dataChannel != nil {
			t.dataChannel.Close()
		}
		if t.peerConnection != nil {
			t.peerConnection.Close()
		}
	})
	t.SetConnected(false)
	return t.BaseTransport.Stop()
}

// Send implements Transport.Send
func (t *MTSLinkWebRTCTransport) Send(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if !t.IsConnected() {
		return fmt.Errorf("not connected")
	}

	cp := make([]byte, len(data))
	copy(cp, data)

	select {
	case t.sendQueue <- cp:
		return nil
	case <-t.done:
		return fmt.Errorf("closed")
	default:
		return fmt.Errorf("queue full")
	}
}

// Receive implements Transport.Receive
func (t *MTSLinkWebRTCTransport) Receive(cb func([]byte)) {
	t.onDataMu.Lock()
	t.onData = cb
	t.onDataMu.Unlock()
}

// IsConnected implements Transport.IsConnected
func (t *MTSLinkWebRTCTransport) IsConnected() bool {
	return t.BaseTransport.IsConnected()
}

func generateDeviceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Helper to read gzip-compressed response
func readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gzReader.Close()
		reader = gzReader
	}
	return io.ReadAll(reader)
}

func (t *MTSLinkWebRTCTransport) addCommonHeaders(req *http.Request) {
	req.Header.Set("User-Agent", webrtcUserAgent)
	req.Header.Set("X-Device-Id", t.deviceID)
	req.Header.Set("X-Device", "Chrome 153.0.0.0")
	req.Header.Set("X-Os", "Linux")
	req.Header.Set("X-App-Version", webrtcAppVersion)
	req.Header.Set("X-Platform", "Web")
	req.Header.Set("Accept", "*/*")
}

// connectLoop handles connection and reconnection
func (t *MTSLinkWebRTCTransport) connectLoop() {
	attempt := 0
	for {
		select {
		case <-t.done:
			return
		default:
		}

		connectedAt := time.Now()
		if err := t.connectAndServe(); err != nil {
			utils.Debugf("[MTS-WebRTC] connection error: %v", err)
		}
		t.SetConnected(false)

		select {
		case <-t.done:
			return
		default:
		}

		// Reset backoff if session was healthy
		next := attempt + 1
		if time.Since(connectedAt) > 30*time.Second {
			next = 0
		}

		d := reconnectBackoff(next)
		utils.Debugf("[MTS-WebRTC] reconnect in %v (attempt %d)", d, next)

		select {
		case <-t.done:
			return
		case <-time.After(d):
		}

		attempt = next
		if attempt > 10 {
			attempt = 10
		}
	}
}

// connectAndServe performs full MTS-Link WebRTC connection flow
func (t *MTSLinkWebRTCTransport) connectAndServe() error {
	utils.Debugf("[MTS-WebRTC] Step 0: Resolving eventSessionId...")
	eventSessionID, err := t.getEventSessionID()
	if err != nil {
		return fmt.Errorf("resolve eventSessionId: %w", err)
	}
	t.eventID = eventSessionID
	utils.Debugf("[MTS-WebRTC] ✓ EventSessionId: %s", eventSessionID)

	// Step 1: Load initial page
	utils.Debugf("[MTS-WebRTC] Step 1: Loading initial page...")
	if err := t.loadInitialPage(); err != nil {
		return fmt.Errorf("load page: %w", err)
	}
	utils.Debugf("[MTS-WebRTC] ✓ Initial page loaded")

	time.Sleep(500 * time.Millisecond)

	// Step 2: Guest login
	utils.Debugf("[MTS-WebRTC] Step 2: Guest login...")
	if err := t.guestLogin(eventSessionID); err != nil {
		return fmt.Errorf("guest login: %w", err)
	}
	utils.Debugf("[MTS-WebRTC] ✓ Guest login OK (UserID: %s)", t.userId)

	time.Sleep(500 * time.Millisecond)

	// Step 3: Confirm login
	utils.Debugf("[MTS-WebRTC] Step 3: Confirming login...")
	if err := t.confirmLogin(); err != nil {
		return fmt.Errorf("confirm login: %w", err)
	}
	utils.Debugf("[MTS-WebRTC] ✓ Login confirmed")

	time.Sleep(500 * time.Millisecond)

	// Step 4: Create conference
	utils.Debugf("[MTS-WebRTC] Step 4: Creating conference...")
	if err := t.createConference(eventSessionID); err != nil {
		return fmt.Errorf("create conference: %w", err)
	}
	utils.Debugf("[MTS-WebRTC] ✓ Conference created (ID: %s)", t.conferenceID)

	time.Sleep(500 * time.Millisecond)

	// Step 5: Register connection session (required before WebSocket)
	utils.Debugf("[MTS-WebRTC] Step 5: Registering connection...")
	if err := t.registerConnection(eventSessionID); err != nil {
		return fmt.Errorf("register connection: %w", err)
	}
	utils.Debugf("[MTS-WebRTC] ✓ Connection registered")

	time.Sleep(500 * time.Millisecond)

	// Step 6: Get join token
	utils.Debugf("[MTS-WebRTC] Step 6: Getting join token...")
	if err := t.getJoinToken(eventSessionID); err != nil {
		return fmt.Errorf("join token: %w", err)
	}
	utils.Debugf("[MTS-WebRTC] ✓ Join token: %s...", t.joinToken[:min(20, len(t.joinToken))])

	time.Sleep(500 * time.Millisecond)

	// Step 7: Join SFU via WebRTC with DataChannel
	utils.Debugf("[MTS-WebRTC] Step 7: Joining SFU via WebRTC...")
	if err := t.joinSFU(eventSessionID); err != nil {
		return fmt.Errorf("join SFU: %w", err)
	}
	utils.Debugf("[MTS-WebRTC] ✓ WebRTC connected!")

	t.SetConnected(true)

	// Wait for DataChannel to open
	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("DataChannel open timeout")
		case <-ticker.C:
			if t.dataChannel != nil && t.dataChannel.ReadyState() == webrtc.DataChannelStateOpen {
				utils.Debugf("[MTS-WebRTC] ✓ DataChannel OPEN")
				goto ready
			}
		case <-t.done:
			return nil
		}
	}

ready:
	// Step 8: Connect WebSocket for signaling (after connection is registered)
	utils.Debugf("[MTS-WebRTC] Step 8: Connecting WebSocket...")
	if err := t.connectWebSocket(); err != nil {
		utils.Debugf("[MTS-WebRTC] WebSocket error (non-fatal): %v", err)
		// Continue without WebSocket - basic functionality still works
	}

	// Start writer loop
	utils.SafeGo("mts-webrtc.writer", func() { t.writerLoop() })

	// Keep connection alive
	<-t.done
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// reconnectBackoff grows from a second up to half a minute, so a transient
// failure does not turn into a busy loop against the conference API.
func reconnectBackoff(n int) time.Duration {
	if n <= 0 {
		return time.Second
	}
	d := time.Duration(1<<uint(min(n, 5))) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}
