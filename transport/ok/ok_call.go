package ok

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// --- same media path as oneme/max_call.go, endpoint via craftPortalEndpoint ---

func (h *CallHandler) SetOnConnected(cb func())     { h.onConnected = cb }
func (h *CallHandler) SetDCInbound(cb func([]byte)) { h.dcInbound = cb }

func (h *CallHandler) Send(data []byte) {
	if useICEInjection {
		h.injectICE(data)
		return
	}
	if h.dc != nil {
		_ = h.dc.Send(data)
	}
}

func (h *CallHandler) injectICE(payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return
	}
	enc := base64.StdEncoding.EncodeToString(payload)
	escaped, _ := json.Marshal(enc)
	msg := fmt.Sprintf(
		`{"command":"transmit-data","sequence":%d,"participantId":%d,"data":{"candidate":{"candidate":%s}},"participantType":"USER"}`,
		h.seq, h.localID, string(escaped),
	)
	h.seq++
	_ = h.conn.WriteMessage(websocket.TextMessage, []byte(msg))
}

func (h *CallHandler) sendAcceptCall() {
	if h.acceptSent {
		return
	}
	h.acceptSent = true
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return
	}
	msg := fmt.Sprintf(
		`{"command":"accept-call","sequence":%d,"mediaSettings":{"isAudioEnabled":true,"isVideoEnabled":false,"isScreenSharingEnabled":false,"isFastScreenSharingEnabled":false,"isAudioSharingEnabled":false,"isAnimojiEnabled":false}}`,
		h.seq,
	)
	h.seq++
	_ = h.conn.WriteMessage(websocket.TextMessage, []byte(msg))
	logInfo("[%s] Accept-call sent", h.tag)
}

func (h *CallHandler) readLoop() {
	defer func() {
		if r := recover(); r != nil {
			logError("recovered in CallHandler.readLoop: %v", r)
		}
	}()
	logInfo("[%s] Signaling connected (PORTAL)", h.tag)
	for {
		_, message, err := h.conn.ReadMessage()
		if err != nil {
			logError("[%s] Signaling disconnected: %v", h.tag, err)
			select {
			case h.reconnectCh <- struct{}{}:
			default:
			}
			return
		}
		text := string(message)
		if strings.Contains(text, "accepted-call") {
			h.callAccepted = true
			continue
		}
		if text == "ping" {
			h.mu.Lock()
			if h.conn != nil {
				_ = h.conn.WriteMessage(websocket.TextMessage, []byte("pong"))
			}
			h.mu.Unlock()
			continue
		}
		if len(text) < 10 {
			continue
		}
		var data map[string]interface{}
		if json.Unmarshal([]byte(text), &data) != nil {
			continue
		}
		if t, _ := data["type"].(string); t == "response" || t == "error" {
			if t == "error" {
				logError("[%s] SIGNALING ERROR: %v", h.tag, data["message"])
			}
			continue
		}
		if pid, ok := data["participantId"].(float64); ok {
			h.remoteID = int64(pid)
		}
		if h.msgHandler != nil {
			go h.msgHandler(text)
		}
	}
}

func (h *CallHandler) createPeerConnection(convParams map[string]interface{}) {
	logInfo("[%s] Creating PeerConnection...", h.tag)
	turn, _ := convParams["turn"].(map[string]interface{})
	var turnURLs []string
	if turn != nil {
		if urls, ok := turn["urls"].([]interface{}); ok {
			for _, u := range urls {
				if s, ok := u.(string); ok {
					turnURLs = append(turnURLs, s)
				}
			}
		}
	}
	username, _ := turn["username"].(string)
	credential, _ := turn["credential"].(string)

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{
			URLs:           turnURLs,
			Username:       username,
			Credential:     credential,
			CredentialType: webrtc.ICECredentialTypePassword,
		}},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		logError("[%s] PC error: %v", h.tag, err)
		return
	}
	h.pc = pc
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logInfo("[%s] Connection: %s", h.tag, s.String())
		if s == webrtc.PeerConnectionStateConnected && h.onConnected != nil {
			h.onConnected()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		h.dc = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if h.dcInbound != nil {
				h.dcInbound(msg.Data)
			}
		})
	})
	ordered := true
	maxRetransmits := uint16(0)
	dc, err := pc.CreateDataChannel("x", &webrtc.DataChannelInit{
		Ordered: &ordered, MaxRetransmits: &maxRetransmits,
	})
	if err == nil {
		h.dc = dc
	}
}

func dialSignaling(endpoint string) (*websocket.Conn, error) {
	return websocket.DefaultDialer.Dial(endpoint, nil)
}

// startOutgoingCall — client role: getCallToken → ws2 PORTAL tgt=start
func startOutgoingCall(client *OkClient, calleeID int64) *CallHandler {
	h := &CallHandler{
		tag:         "CALLER",
		role:        "caller",
		seq:         1,
		reconnectCh: make(chan struct{}, 1),
	}
	h.msgHandler = func(text string) {
		var data map[string]interface{}
		_ = json.Unmarshal([]byte(text), &data)
		if cp, ok := data["conversationParams"].(map[string]interface{}); ok {
			h.createPeerConnection(cp)
			return
		}
		d, _ := data["data"].(map[string]interface{})
		if d == nil {
			return
		}
		if c, ok := d["candidate"].(map[string]interface{}); ok && useICEInjection {
			candidateStr, _ := c["candidate"].(string)
			decode, err := base64.StdEncoding.DecodeString(candidateStr)
			if err == nil && h.dcInbound != nil {
				h.dcInbound(decode)
			}
		}
	}

	go func() {
		for {
			params, err := client.GetCallToken(calleeID)
			if err != nil {
				logError("[CALLER] GetCallToken: %v — retry in 2s", err)
				time.Sleep(2 * time.Second)
				continue
			}
			if params.UserID == "" {
				params.UserID = fmt.Sprintf("%d", calleeID)
			}
			endpoint := craftPortalEndpoint(params, "start")
			logInfo("[CALLER] dial %s", endpoint)
			conn, err := dialSignaling(endpoint)
			if err != nil {
				logError("[CALLER] dial: %v", err)
				time.Sleep(2 * time.Second)
				continue
			}
			h.mu.Lock()
			h.conn = conn
			h.mu.Unlock()
			go h.readLoop()
			<-h.reconnectCh
			logInfo("[CALLER] reconnect...")
			time.Sleep(1 * time.Second)
		}
	}()
	return h
}

// startIncomingListener — exit role: wait OK incoming → ws2 PORTAL tgt=accept
func startIncomingListener(client *OkClient) *CallHandler {
	h := &CallHandler{
		tag:         "RECEIVER",
		role:        "receiver",
		seq:         1,
		reconnectCh: make(chan struct{}, 1),
	}
	h.msgHandler = func(text string) {
		var data map[string]interface{}
		_ = json.Unmarshal([]byte(text), &data)
		if cp, ok := data["conversationParams"].(map[string]interface{}); ok {
			h.createPeerConnection(cp)
			return
		}
		d, _ := data["data"].(map[string]interface{})
		if d == nil {
			return
		}
		if c, ok := d["candidate"].(map[string]interface{}); ok && useICEInjection {
			candidateStr, _ := c["candidate"].(string)
			decode, err := base64.StdEncoding.DecodeString(candidateStr)
			if err == nil && h.dcInbound != nil {
				h.dcInbound(decode)
			}
		}
	}

	go func() {
		logInfo("[RECEIVER] Waiting for OK/PORTAL calls...")
		for {
			params, err := client.WaitIncomingCall()
			if err != nil {
				logError("[RECEIVER] %v", err)
				// Do not spin forever on unimplemented API — sleep and retry
				// so a later HAR-filled WaitIncomingCall can work without restart.
				time.Sleep(10 * time.Second)
				continue
			}
			_ = client.AcceptCall(params.ConvID)
			endpoint := craftPortalEndpoint(params, "accept")
			logInfo("[RECEIVER] dial %s", endpoint)
			conn, err := dialSignaling(endpoint)
			if err != nil {
				logError("[RECEIVER] dial: %v", err)
				continue
			}
			h.mu.Lock()
			h.conn = conn
			h.acceptSent = false
			h.mu.Unlock()
			go h.readLoop()
			time.Sleep(1 * time.Second)
			h.sendAcceptCall()
			<-h.reconnectCh
		}
	}()
	return h
}
