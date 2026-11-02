package mtslink

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// connectWebSocket establishes Engine.IO/Socket.IO WebSocket connection
func (t *MTSLinkWebRTCTransport) connectWebSocket() error {
	wsURL := "wss://msg-edge-12.webinar.ru/engine.io/?EIO=3&transport=websocket"

	headers := map[string][]string{
		"Origin":     {"https://my.mts-link.ru"},
		"User-Agent": {webrtcUserAgent},
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	utils.Debugf("[MTS-WebRTC] Connecting to WebSocket: %s", wsURL)
	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}

	t.wsMu.Lock()
	t.wsConn = conn
	t.wsMu.Unlock()

	utils.Debugf("[MTS-WebRTC] ✓ WebSocket connected")

	// Start WebSocket reader
	utils.SafeGo("mts-webrtc.ws-reader", func() { t.wsReaderLoop() })

	// Start ping/pong keepalive
	utils.SafeGo("mts-webrtc.ws-ping", func() { t.wsPingLoop() })

	// Wait for Engine.IO handshake
	time.Sleep(100 * time.Millisecond)

	// Subscribe to channels in two steps (like browser does)
	if err := t.subscribeChannelsStep1(); err != nil {
		return fmt.Errorf("subscribe step 1: %w", err)
	}

	time.Sleep(50 * time.Millisecond)

	if err := t.subscribeChannelsStep2(); err != nil {
		return fmt.Errorf("subscribe step 2: %w", err)
	}

	return nil
}

// wsPingLoop sends periodic pings to keep WebSocket alive
func (t *MTSLinkWebRTCTransport) wsPingLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.wsMu.Lock()
			if t.wsConn != nil {
				// Send Engine.IO ping ("2")
				if err := t.wsConn.WriteMessage(websocket.TextMessage, []byte("2")); err != nil {
					utils.Debugf("[MTS-WebRTC] WebSocket ping failed: %v", err)
					t.wsMu.Unlock()
					return
				}
			}
			t.wsMu.Unlock()
		}
	}
}

// subscribeChannelsStep1 subscribes to initial channels (cuid, csid)
func (t *MTSLinkWebRTCTransport) subscribeChannelsStep1() error {
	channels := []string{
		fmt.Sprintf("cuid_%s_261436ba1d5a29947922a237666dd2dc", t.userId),
		fmt.Sprintf("csid_%s_81f8f70951138be04f74d4cec468deaa", t.sessionID),
	}

	return t.sendSubscribe(channels)
}

// subscribeChannelsStep2 subscribes to all channels including event-specific ones
func (t *MTSLinkWebRTCTransport) subscribeChannelsStep2() error {
	channels := []string{
		fmt.Sprintf("cuid_%s_261436ba1d5a29947922a237666dd2dc", t.userId),
		fmt.Sprintf("csid_%s_81f8f70951138be04f74d4cec468deaa", t.sessionID),
		fmt.Sprintf("cuiesid_%s_%s_ed35f80178206c58b9070519b48264fd", t.userId, t.eventID),
		fmt.Sprintf("ceid_%s_e2e51a85ed335d97d829e7268e95b554", t.eventID),
		fmt.Sprintf("cesid_%s_1e8b36fd73d3b8285ce6458b50c457a8", t.eventID),
		fmt.Sprintf("cpsesid_%s_%s_%s_9dab830747801072faecdf5ce3f13c14", t.eventID, t.participationID, t.sessionID),
	}

	return t.sendSubscribe(channels)
}

// sendSubscribe sends a subscribe message with the given channels
func (t *MTSLinkWebRTCTransport) sendSubscribe(channels []string) error {
	// Socket.IO v4 format: 4{"name":"subscribe","data":{...}}
	subscribeData := map[string]interface{}{
		"name": "subscribe",
		"data": map[string]interface{}{
			"version":  "0.3",
			"channels": channels,
			"cursor":   nil,
		},
	}

	msgBytes, err := json.Marshal(subscribeData)
	if err != nil {
		return fmt.Errorf("marshal subscribe: %w", err)
	}

	// Engine.IO message format: "4" (Socket.IO v4 event) + JSON object
	eioMsg := "4" + string(msgBytes)

	t.wsMu.Lock()
	defer t.wsMu.Unlock()

	if err := t.wsConn.WriteMessage(websocket.TextMessage, []byte(eioMsg)); err != nil {
		return fmt.Errorf("write subscribe: %w", err)
	}

	utils.Debugf("[MTS-WebRTC] ✓ Subscribed to %d channels", len(channels))
	return nil
}

// wsReaderLoop reads and processes WebSocket messages
func (t *MTSLinkWebRTCTransport) wsReaderLoop() {
	defer func() {
		t.wsMu.Lock()
		if t.wsConn != nil {
			t.wsConn.Close()
			t.wsConn = nil
		}
		t.wsMu.Unlock()
	}()

	for {
		select {
		case <-t.done:
			return
		default:
		}

		t.wsMu.Lock()
		conn := t.wsConn
		t.wsMu.Unlock()

		if conn == nil {
			return
		}

		msgType, data, err := conn.ReadMessage()
		if err != nil {
			// Check if it's a normal close
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				utils.Debugf("[MTS-WebRTC] WebSocket closed normally")
			} else {
				utils.Debugf("[MTS-WebRTC] WebSocket read error: %v", err)
			}
			return
		}

		if msgType == websocket.TextMessage {
			t.handleWSMessage(string(data))
		}
	}
}

// handleWSMessage processes incoming WebSocket messages
func (t *MTSLinkWebRTCTransport) handleWSMessage(msg string) {
	// Engine.IO message format:
	// "0" = open
	// "2" = ping
	// "3" = pong
	// "4" = Socket.IO v4 event (JSON object format)
	// "40" = connect
	// "41" = disconnect
	// "42" = Socket.IO v3 event (JSON array format)

	if len(msg) == 0 {
		return
	}

	// Handle Engine.IO control messages
	if msg == "0" || strings.HasPrefix(msg, "0{") {
		utils.Debugf("[MTS-WebRTC] WebSocket handshake: %s", msg[:min(100, len(msg))])
		return
	}

	if msg == "2" {
		// Ping - respond with pong
		t.wsMu.Lock()
		if t.wsConn != nil {
			t.wsConn.WriteMessage(websocket.TextMessage, []byte("3"))
		}
		t.wsMu.Unlock()
		return
	}

	if msg == "3" {
		// Pong
		return
	}

	// Handle Socket.IO v4 events (4 prefix with JSON object)
	if strings.HasPrefix(msg, "4{") {
		jsonData := msg[1:] // Strip "4" prefix
		t.handleSocketIOv4Event(jsonData)
		return
	}

	// Handle Socket.IO v3 events (42 prefix with JSON array)
	if strings.HasPrefix(msg, "42") {
		jsonData := msg[2:] // Strip "42" prefix
		t.handleSocketIOv3Event(jsonData)
		return
	}

	utils.Debugf("[MTS-WebRTC] WebSocket unknown message: %s", msg[:min(100, len(msg))])
}

// handleSocketIOv4Event processes Socket.IO v4 event messages (JSON object format)
func (t *MTSLinkWebRTCTransport) handleSocketIOv4Event(jsonData string) {
	// Socket.IO v4 format: {"name":"event_name","data":{...}}
	var event struct {
		Name string          `json:"name"`
		Data json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
		utils.Debugf("[MTS-WebRTC] Failed to parse v4 event: %v", err)
		return
	}

	switch event.Name {
	case "message":
		t.handleMessageEvent(event.Data)
	default:
		utils.Debugf("[MTS-WebRTC] WebSocket event: %s", event.Name)
	}
}

// handleSocketIOv3Event processes Socket.IO v3 event messages (JSON array format)
func (t *MTSLinkWebRTCTransport) handleSocketIOv3Event(jsonData string) {
	// Socket.IO v3 format: [event_name, data]
	var eventArray []json.RawMessage
	if err := json.Unmarshal([]byte(jsonData), &eventArray); err != nil {
		utils.Debugf("[MTS-WebRTC] Failed to parse v3 event array: %v", err)
		return
	}

	if len(eventArray) < 2 {
		return
	}

	var eventName string
	if err := json.Unmarshal(eventArray[0], &eventName); err != nil {
		return
	}

	switch eventName {
	case "message":
		t.handleMessageEvent(eventArray[1])
	default:
		utils.Debugf("[MTS-WebRTC] WebSocket event: %s", eventName)
	}
}

// handleMessageEvent processes message events containing conference updates
func (t *MTSLinkWebRTCTransport) handleMessageEvent(data json.RawMessage) {
	var msgData struct {
		Messages []string `json:"messages"`
	}

	if err := json.Unmarshal(data, &msgData); err != nil {
		return
	}

	for _, msgStr := range msgData.Messages {
		var msg struct {
			Key  string          `json:"key"`
			Data json.RawMessage `json:"data"`
		}

		if err := json.Unmarshal([]byte(msgStr), &msg); err != nil {
			continue
		}

		switch msg.Key {
		case "conference.add":
			utils.Debugf("[MTS-WebRTC] New conference peer added")
			// TODO: Handle new peer for P2P DataChannel setup
		case "conference.update":
			utils.Debugf("[MTS-WebRTC] Conference updated")
		case "screensharing.update":
			utils.Debugf("[MTS-WebRTC] Screensharing updated")
		}
	}
}

// generateChannelHash generates a simple hash for channel names
// In real implementation, this should match MTS-Link's hashing algorithm
func generateChannelHash(parts ...string) string {
	// Placeholder - need to reverse-engineer actual hash algorithm
	return "placeholder_hash"
}
