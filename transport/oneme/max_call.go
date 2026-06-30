package oneme

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

var (
	useICEInjection = true
)

func (h *CallHandler) SetOnConnected(cb func()) { h.onConnected = cb }

func (h *CallHandler) Send(data []byte) {
	if useICEInjection {
		h.injectICE(data)
	} else {
		if h.dc != nil {
			h.dc.Send(data)
		}
	}
}

func (h *CallHandler) readLoop(callback func()) {
	logInfo("[%s] Signaling connected", h.tag)
	defer callback()
	for {
		_, message, err := h.conn.ReadMessage()
		if err != nil {
			logError("[%s] Signaling disconnected: %v", h.tag, err)
			return
		}
		text := string(message)

		if strings.Contains(text, "accepted-call") {
			fmt.Println("call accepted")
			h.callAccepted = true
			continue
		}

		if text == "ping" {
			h.mu.Lock()
			h.conn.WriteMessage(websocket.TextMessage, []byte("pong"))
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
		if t, _ := data["type"].(string); t == "response" {
			continue
		}
		if t, _ := data["type"].(string); t == "error" {
			logError("[%s] SIGNALING ERROR: %v", h.tag, data["message"])
			continue
		}

		if pid, ok := data["participantId"].(float64); ok {
			h.remoteID = int64(pid)
		}
		go h.msgHandler(text)
	}
}

func (h *CallHandler) sendAcceptCall() {
	if h.acceptSent {
		return
	}
	h.acceptSent = true
	h.mu.Lock()
	msg := fmt.Sprintf(`{"command":"accept-call","sequence":%d,"mediaSettings":{"isAudioEnabled":true,"isVideoEnabled":false,"isScreenSharingEnabled":false,"isFastScreenSharingEnabled":false,"isAudioSharingEnabled":false,"isAnimojiEnabled":false}}`, h.seq)
	h.seq++
	h.mu.Unlock()
	h.conn.WriteMessage(websocket.TextMessage, []byte(msg))
	logInfo("[%s] Accept-call sent", h.tag)
}

func (h *CallHandler) createPeerConnection(convParams map[string]interface{}) {
	logInfo("[%s] Creating PeerConnection...", h.tag)
	turn := convParams["turn"].(map[string]interface{})
	stun := convParams["stun"].(map[string]interface{})
	var stunURLs, turnURLs []string
	if urls, ok := stun["urls"].([]interface{}); ok && len(urls) > 0 {
		stunURLs = []string{urls[0].(string)}
	}
	if urls, ok := turn["urls"].([]interface{}); ok {
		for _, u := range urls {
			turnURLs = append(turnURLs, u.(string))
		}
	}
	username, _ := turn["username"].(string)
	credential, _ := turn["credential"].(string)
	logInfo("[%s] STUN: %v  TURN: %v", h.tag, stunURLs, turnURLs)

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs:           turnURLs,
				Username:       username,
				Credential:     credential,
				CredentialType: webrtc.ICECredentialTypePassword,
			},
		},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		logError("[%s] ERROR creating PC: %v", h.tag, err)
		return
	}
	h.pc = pc

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			jsonC, _ := json.Marshal(c.ToJSON())
			logInfo("[%s] Local ICE: %s", h.tag, string(jsonC))
			h.sendICE(string(jsonC))
		} else {
			logInfo("[%s] ICE gathering complete", h.tag)
		}
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		logInfo("[%s] ICE: %s", h.tag, s.String())
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logInfo("[%s] Connection: %s", h.tag, s.String())
		if s == webrtc.PeerConnectionStateConnected && h.onConnected != nil {
			logInfo("[%s] *** CONNECTED! ***", h.tag)
			h.onConnected()
		}
	})
	pc.OnSignalingStateChange(func(s webrtc.SignalingState) {
		logInfo("[%s] Signaling: %s", h.tag, s.String())
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dcID := uint16(0)
		if dc.ID() != nil {
			dcID = *dc.ID()
		}
		logInfo("[%s] Remote DC: %s (id=%d)", h.tag, dc.Label(), dcID)
		h.dc = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			logInfo("[%s] RECV: %s", h.tag, string(msg.Data))
			h.dcInbound(msg.Data)
		})
	})

	ordered := true
	maxRetransmits := uint16(0)
	dc, err := pc.CreateDataChannel("x", &webrtc.DataChannelInit{
		Ordered: &ordered, MaxRetransmits: &maxRetransmits,
	})
	if err != nil {
		logError("[%s] ERROR creating DC: %v", h.tag, err)
		return
	}
	if dc == nil {
		logError("[%s] DC is nil", h.tag)
		return
	}
	h.dc = dc
	dcID := uint16(0)
	if dc.ID() != nil {
		dcID = *dc.ID()
	}
	logInfo("[%s] DataChannel: label=%s id=%d", h.tag, dc.Label(), dcID)
	dc.OnOpen(func() { logInfo("[%s] DC opened", h.tag) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		h.dcInbound(msg.Data)
	})
}

func (h *CallHandler) sendSDP(sdp string, sdpType string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	escaped, _ := json.Marshal(sdp)
	msg := fmt.Sprintf(`{"command":"transmit-data","sequence":%d,"participantId":%d,"data":{"sdp":{"type":"%s","sdp":%s},"animojiVersion":1},"participantType":"USER"}`,
		h.seq, h.localID, sdpType, string(escaped))
	h.seq++
	logInfo("[%s] Sent SDP %s (%d bytes)", h.tag, sdpType, len(sdp))
	h.conn.WriteMessage(websocket.TextMessage, []byte(msg))
}

func (h *CallHandler) injectICE(payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	type ice struct {
		Candidate string `json:"candidate"`
	}
	structPayload := ice{Candidate: base64.StdEncoding.EncodeToString(payload)}
	escaped, _ := json.Marshal(structPayload.Candidate)
	msg := fmt.Sprintf(`{"command":"transmit-data","sequence":%d,"participantId":%d,"data":{"candidate":{"candidate":%s}},"participantType":"USER"}`,
		h.seq, h.localID, string(escaped))
	h.seq++
	h.conn.WriteMessage(websocket.TextMessage, []byte(msg))
}

func (h *CallHandler) sendICE(candidateJSON string) {
	if useICEInjection {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	var ice struct {
		Candidate string `json:"candidate"`
	}
	json.Unmarshal([]byte(candidateJSON), &ice)
	escaped, _ := json.Marshal(ice.Candidate)
	msg := fmt.Sprintf(`{"command":"transmit-data","sequence":%d,"participantId":%d,"data":{"candidate":{"candidate":%s}},"participantType":"USER"}`,
		h.seq, h.localID, string(escaped))
	h.seq++
	h.conn.WriteMessage(websocket.TextMessage, []byte(msg))
}

func (h *CallHandler) addICECandidate(c map[string]interface{}) {
	if h.pc == nil {
		return
	}
	candidateStr, _ := c["candidate"].(string)
	sdpMid, _ := c["sdpMid"].(string)
	sdpMLineIndex := uint16(0)
	if idx, ok := c["sdpMLineIndex"].(float64); ok {
		sdpMLineIndex = uint16(idx)
	}
	if err := h.pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate: candidateStr, SDPMid: &sdpMid, SDPMLineIndex: &sdpMLineIndex,
	}); err != nil {
		logError("[%s] ICE error: %v", h.tag, err)
	}
}

func (h *CallHandler) bufferOrAddICE(c map[string]interface{}) {
	if !h.hasRemoteDesc {
		h.pendingCandidates = append(h.pendingCandidates, c)
		logInfo("[%s] Buffered ICE (%d total)", h.tag, len(h.pendingCandidates))
	} else {
		h.addICECandidate(c)
	}
}

func (h *CallHandler) flushPendingCandidates() {
	if len(h.pendingCandidates) == 0 {
		return
	}
	logInfo("[%s] Flushing %d buffered ICE", h.tag, len(h.pendingCandidates))
	for _, c := range h.pendingCandidates {
		h.addICECandidate(c)
	}
	h.pendingCandidates = nil
}

func (h *CallHandler) handleSDP(sdpType string, sdpStr string) {
	if h.pc == nil {
		return
	}
	logInfo("[%s] handleSDP: %s (%d bytes)", h.tag, sdpType, len(sdpStr))

	switch sdpType {
	case "offer":
		logInfo("[%s] Setting remote offer...", h.tag)
		if err := h.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer, SDP: sdpStr,
		}); err != nil {
			logError("[%s] ERROR: %v", h.tag, err)
			return
		}
		time.Sleep(300 * time.Millisecond)
		h.hasRemoteDesc = true
		h.flushPendingCandidates()

		logInfo("[%s] Creating answer...", h.tag)
		answer, err := h.pc.CreateAnswer(nil)
		if err != nil {
			logError("[%s] ERROR: %v", h.tag, err)
			return
		}
		h.pc.SetLocalDescription(answer)
		h.sendSDP(answer.SDP, "answer")

	case "answer":
		logInfo("[%s] Setting remote answer...", h.tag)
		if err := h.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer, SDP: sdpStr,
		}); err != nil {
			logError("[%s] ERROR: %v", h.tag, err)
			return
		}
		h.hasRemoteDesc = true
		h.flushPendingCandidates()
	}
}

func startOutgoingCall(client *MaxClient, calleeID int64) *CallHandler {
	h := &CallHandler{tag: "CALLER"}
	h.seq = 1
	h.msgHandler = func(text string) {
		var data map[string]interface{}
		json.Unmarshal([]byte(text), &data)

		if h.localID == 0 {
			if conv, ok := data["conversation"].(map[string]interface{}); ok {
				if parts, ok := conv["participants"].([]interface{}); ok {
					for _, p := range parts {
						part := p.(map[string]interface{})
						roles, _ := part["roles"].([]interface{})
						isCreator := false
						for _, r := range roles {
							if r.(string) == "CREATOR" {
								isCreator = true
							}
						}
						if !isCreator {
							h.localID = int64(part["id"].(float64))
							logInfo("[%s] Local ID: %d", h.tag, h.localID)
						}
					}
				}
			}
		}

		if cp, ok := data["conversationParams"].(map[string]interface{}); ok {
			logInfo("[%s] conversationParams - creating offer", h.tag)
			for {
				time.Sleep(1 * time.Second)
				fmt.Println("waiting for accept ...")
				if h.callAccepted {
					break
				}
			}
			h.createPeerConnection(cp)
			time.Sleep(200 * time.Millisecond)
			offer, err := h.pc.CreateOffer(nil)
			if err != nil {
				logError("[%s] ERROR: %v", h.tag, err)
				return
			}
			h.pc.SetLocalDescription(offer)
			h.sendSDP(offer.SDP, "offer")
			return
		}
		d, _ := data["data"].(map[string]interface{})
		if d == nil {
			return
		}
		if sdp, ok := d["sdp"].(map[string]interface{}); ok {
			sdpType, _ := sdp["type"].(string)
			if sdpType == "answer" {
				h.handleSDP(sdpType, sdp["sdp"].(string))
			}
			return
		}
		if c, ok := d["candidate"].(map[string]interface{}); ok {
			if useICEInjection {
				candidateStr, _ := c["candidate"].(string)
				decode, _ := base64.StdEncoding.DecodeString(candidateStr)
				h.dcInbound(decode)
			} else {
				h.bufferOrAddICE(c)
			}
		}
	}

	logInfo("[CALLER] Calling %d", calleeID)
	resp, _ := client.invoke(78, map[string]interface{}{
		"conversationId": genUUID(),
		"calleeIds":      []int64{calleeID},
		"internalParams": fmt.Sprintf(`{"deviceId":"%s","sdkVersion":"2.8.9","clientAppKey":"CNHIJPLGDIHBABABA","platform":"WEB","protocolVersion":5,"domainId":"","capabilities":"2A03F"}`, client.deviceID),
		"isVideo":        false,
	})
	var payload map[string]interface{}
	json.Unmarshal(resp.Payload, &payload)
	paramsStr, _ := payload["internalCallerParams"].(string)
	var params InternalCallerParams
	json.Unmarshal([]byte(paramsStr), &params)

	endpoint := params.Endpoint + "&platform=WEB&appVersion=1.1&version=5&device=browser&capabilities=2A03F&clientType=ONE_ME&tgt=start"
	conn, _, _ := websocket.DefaultDialer.Dial(endpoint, nil)
	h.conn = conn
	go h.readLoop(func() {
		if !useICEInjection {
			return
		}

		fmt.Println("Signalling is dead, calling again bc of injectICE turned on")
		startOutgoingCall(client, calleeID)
	})
	return h
}

func startIncomingListener(client *MaxClient) *CallHandler {
	h := &CallHandler{tag: "RECEIVER"}
	h.seq = 1
	h.msgHandler = func(text string) {
		var data map[string]interface{}
		json.Unmarshal([]byte(text), &data)

		if h.localID == 0 {
			if conv, ok := data["conversation"].(map[string]interface{}); ok {
				if parts, ok := conv["participants"].([]interface{}); ok {
					for _, p := range parts {
						part := p.(map[string]interface{})
						roles, _ := part["roles"].([]interface{})
						isCreator := false
						for _, r := range roles {
							if r.(string) == "CREATOR" {
								isCreator = true
							}
						}
						if isCreator {
							h.localID = int64(part["id"].(float64))
							logInfo("[%s] Local ID: %d", h.tag, h.localID)
						}
					}
				}
			}
		}

		if cp, ok := data["conversationParams"].(map[string]interface{}); ok {
			h.createPeerConnection(cp)
			return
		}
		d, _ := data["data"].(map[string]interface{})
		if d == nil {
			return
		}
		if sdp, ok := d["sdp"].(map[string]interface{}); ok {
			h.handleSDP(sdp["type"].(string), sdp["sdp"].(string))
			return
		}
		if c, ok := d["candidate"].(map[string]interface{}); ok {

			if useICEInjection {
				candidateStr, _ := c["candidate"].(string)
				decode, _ := base64.StdEncoding.DecodeString(candidateStr)
				h.dcInbound(decode)
			} else {
				h.bufferOrAddICE(c)
			}
		}
	}

	client.SetEventCallback(func(p MaxPacket) {
		if p.Opcode == 137 {
			logInfo("[RECEIVER] Incoming call!")
			var payload map[string]interface{}
			json.Unmarshal(p.Payload, &payload)
			convID, _ := payload["conversationId"].(string)
			vcp, _ := payload["vcp"].(string)
			callDetails, err := decodeCallDetails(vcp)
			if err != nil {
				logError("[RECEIVER] Decode error: %v", err)
				return
			}

			endpoint := craftEndpoint(convID, callDetails)
			logInfo("[RECEIVER] Initial endpoint: %s", endpoint)

			conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
			if err != nil {
				logError("[RECEIVER] Connect error: %v", err)
				return
			}
			h.conn = conn
			go h.readLoop(func() {
				if !useICEInjection {
					return
				}

				fmt.Println("Signalling is dead. " +
					"Gonna kill myself now")
			})
			// accept-call will be sent after reconnection (in readLoop) or after timeout
			go func() {
				time.Sleep(1 * time.Second)
				if !h.acceptSent {
					h.sendAcceptCall()
				}
			}()
		}
	})
	logInfo("[RECEIVER] Waiting for calls...")
	return h
}
