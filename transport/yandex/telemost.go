// Package yandex: Telemost transport.
//
// Tunnels packets through a Yandex Telemost conference: HTTP join gives
// the signaling WS URL + room/peer/credentials; WS carries SDP/ICE; two
// WebRTC PeerConnections (Publisher + Subscriber) carry the actual bytes
// inside VP8 video frames built by CanvasVideoGenerator/Decoder.
package yandex

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

var (
	tmZstdEnc *zstd.Encoder
	tmZstdDec *zstd.Decoder
)

func init() {
	var err error
	tmZstdEnc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		panic(fmt.Sprintf("telemost zstd encoder init: %v", err))
	}
	tmZstdDec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20))
	if err != nil {
		panic(fmt.Sprintf("telemost zstd decoder init: %v", err))
	}
}

const telemostUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"

func roleStr(isExitNode bool) string {
	if isExitNode {
		return "EXIT"
	}
	return "CLIENT"
}

// TelemostTransport carries OpenFlux packets over a Yandex Telemost
// conference using VP8 keyframes as the wire format.
type TelemostTransport struct {
	*transport.BaseTransport

	cookies       string
	conferenceURL string
	userID        string
	isExitNode    bool

	roomID           string
	peerID           string
	lane             *stripeLane // non-nil when part of a telemost group
	participantID    string
	credentials      string
	mediaServerURL   string
	sessionID        string
	peerSessionID    string
	clientInstanceID string
	idempotencyKey   string

	conn     *websocket.Conn
	connMu   sync.RWMutex
	wsSendMu sync.Mutex

	// Two PeerConnections: Telemost uses SEPARATE offer/answer.
	publisherPC  *webrtc.PeerConnection
	subscriberPC *webrtc.PeerConnection
	sharingTrack *TelemostTrack
	rtxMu        sync.Mutex
	uplinkLoss   atomic.Uint32 // last RR fraction lost, /256
	sfuGot       atomic.Uint64 // packets the SFU says it received (RR ext seq - lost)
	rtxRing      [rtxRingSize]rtxEntry
	rtxStats     struct{ asked, resent, missed atomic.Int64 }
	publishPlan  *videoTrackPlan

	sharingDC    *webrtc.DataChannel
	vp8Payloader codecs.VP8Payloader
	encoder      *frameEncoder

	// audioTrack carries tunnel data over a fake Opus track instead of the
	// fake video one when shareCarrier is "audio" (canvas_audio.go). It
	// rides the same publisherPC as sharingTrack, on the audio transceiver
	// every publish already creates, so trackReady (set from publisherPC's
	// own connection state) applies to it too - it is not video-specific
	// despite the name.
	audioTrack      *TelemostTrack
	audioSeq        atomic.Uint32
	audioTS         atomic.Uint32
	audioFrameMu    sync.Mutex
	audioFrameQueue [][]byte

	// What this participant advertises to the SFU. Screen sharing is
	// single-occupant in a Telemost room - only one participant can hold the
	// share slot - so a two-node tunnel cannot have both ends claim it. Camera
	// video has no such limit. TELEMOST_PUBLISH_KIND selects between them.
	publishKind publishKind

	// shareCodecMime is the fake video codec this participant publishes.
	// TELEMOST_CODEC selects it (default VP8, see parseShareCodec). The
	// receiving side never needs to be told which one to expect - Pion
	// reports the negotiated codec per track (track.Codec().MimeType), and
	// readSharingVideoTrack picks a decoder from that.
	shareCodecMime string

	// shareCarrier is "video" (default) or "audio" (TELEMOST_CARRIER, see
	// parseShareCarrier): which track tunnel data actually rides on. The
	// video transceiver and its whole slot/kind machinery still run
	// unconditionally either way - carrier only decides where enqueueFrame
	// sends real data; the other track just keeps emitting its own filler.
	shareCarrier string

	trackReady    atomic.Bool
	shareSSRC     uint32
	sharePayloadT uint8
	shareRIDExtID int
	shareMID      string
	publishMode   publishMode
	shareSeq      atomic.Uint32
	shareTS       atomic.Uint32
	pubSeq        atomic.Int32
	subSeq        atomic.Int32
	sdpSemantics  webrtc.SDPSemantics

	// pingIntervalMs is the WS heartbeat cadence, in milliseconds. It starts
	// at defaultPingIntervalMs and is overwritten once serverHello states
	// what the server actually expects (pingPongConfiguration.pingInterval).
	pingIntervalMs atomic.Int64

	serverConfig struct {
		sessionSecret           string
		sfuPeerInitializationID string
		pingInterval            int
		ackTimeout              int
		videoCodecConfig        map[string]interface{}
		videoLayersConfig       map[string]interface{}
		rtcConfiguration        map[string]interface{}
		iceServers              []webrtc.ICEServer
		servingComponents       []interface{}
	}

	slotKey atomic.Uint32

	writeQueue chan []byte

	// frameMu guards frameQueue, the frames waiting for the next tick of the
	// publish clock (publishFPS). Tunnel bytes are buffered here rather than
	// written the moment they arrive, so that the SFU sees one frame every
	// tick whether or not there is traffic. See runPublisherKeepalive for why.
	frameMu    sync.Mutex
	frameQueue [][]byte

	lastSigPayload atomic.Value
	lastSigAt      atomic.Int64

	batchSize     int
	batchTimeout  time.Duration
	batchMaxBytes int

	stats struct {
		PacketsSent     atomic.Uint64
		PacketsRecv     atomic.Uint64
		BytesSent       atomic.Uint64
		BytesReceived   atomic.Uint64
		BatchesSent     atomic.Uint64
		PacketsBatched  atomic.Uint64
		Reconnects      atomic.Uint64
		RTPSendFailed   atomic.Uint64
		RTPSendSuccess  atomic.Uint64
		VideoFramesSent atomic.Uint64
		VideoFramesRecv atomic.Uint64
	}

	lastSendTime atomic.Int64
	lastRecvTime atomic.Int64
}

type TelemostConnectionResponse struct {
	ConnectionType      string                 `json:"connection_type"`
	URI                 string                 `json:"uri"`
	RoomID              string                 `json:"room_id"`
	SafeRoomID          string                 `json:"safe_room_id"`
	PeerID              string                 `json:"peer_id"`
	ClientConfiguration map[string]interface{} `json:"client_configuration"`
	ConferenceState     map[string]interface{} `json:"conference_state"`
	MediaPlatform       string                 `json:"media_platform"`
	SessionID           string                 `json:"session_id"`
	PeerSessionID       string                 `json:"peer_session_id"`
	Credentials         string                 `json:"credentials"`
	WSURI               string                 `json:"ws_uri"`
}

type TelemostHello struct {
	UID   string            `json:"uid"`
	Hello TelemostHelloData `json:"hello"`
}

type TelemostHelloData struct {
	RoomID                 string                 `json:"roomId"`
	ParticipantID          string                 `json:"participantId"`
	Credentials            string                 `json:"credentials"`
	ServiceName            string                 `json:"serviceName"`
	SendAudio              bool                   `json:"sendAudio"`
	SendVideo              bool                   `json:"sendVideo"`
	SendSharing            bool                   `json:"sendSharing"`
	DisablePublisher       bool                   `json:"disablePublisher"`
	DisableSubscriber      bool                   `json:"disableSubscriber"`
	DisableSubscriberAudio bool                   `json:"disableSubscriberAudio"`
	ParticipantAttributes  map[string]interface{} `json:"participantAttributes"`
	ParticipantMeta        map[string]interface{} `json:"participantMeta"`
	SDKInfo                map[string]interface{} `json:"sdkInfo"`
	SDKInitializationID    string                 `json:"sdkInitializationId"`
	CapabilitiesOffer      map[string]interface{} `json:"capabilitiesOffer"`
}

type TelemostUpdateMe struct {
	UID      string                 `json:"uid"`
	UpdateMe map[string]interface{} `json:"updateMe"`
}

func NewTelemostTransport(cookies, conferenceURL string, isExitNode bool, config transport.TransportConfig) *TelemostTransport {
	return &TelemostTransport{
		BaseTransport:    transport.NewBaseTransport(config),
		cookies:          cookies,
		conferenceURL:    conferenceURL,
		isExitNode:       isExitNode,
		publishKind:      parsePublishKind(os.Getenv("TELEMOST_PUBLISH_KIND")),
		shareCodecMime:   parseShareCodec(os.Getenv("TELEMOST_CODEC")),
		shareCarrier:     parseShareCarrier(os.Getenv("TELEMOST_CARRIER")),
		writeQueue:       make(chan []byte, config.MaxQueueSize),
		clientInstanceID: uuid.New().String(),
		idempotencyKey:   uuid.New().String(),
		batchSize:        1,
		batchTimeout:     1 * time.Microsecond,
		batchMaxBytes:    2000,
	}
}

func (t *TelemostTransport) Start() error {
	utils.Debugf("[Telemost] Starting transport...")
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}
	if t.conferenceURL == "" {
		t.userID = extractUserIDFromCookie(t.cookies)
		if t.userID == "" {
			return fmt.Errorf("failed to extract user ID from cookies")
		}
	} else if t.cookies != "" {
		t.userID = extractUserIDFromCookie(t.cookies)
	}
	if err := t.getConnectionInfo(); err != nil {
		return fmt.Errorf("failed to get connection info: %w", err)
	}
	t.connectWS(0)
	utils.SafeGo("telemost.writer", t.writerLoop)
	utils.SafeGo("telemost.keepalive", t.keepAliveLoop)
	utils.Debugf("[Telemost] Start() complete")
	return nil
}

func (t *TelemostTransport) Stop() error {
	t.connMu.Lock()
	if t.conn != nil {
		_ = t.conn.Close()
		t.conn = nil
	}
	t.connMu.Unlock()
	if t.publisherPC != nil {
		_ = t.publisherPC.Close()
	}
	if t.subscriberPC != nil {
		_ = t.subscriberPC.Close()
	}
	t.SetConnected(false)
	return t.BaseTransport.Stop()
}

func (t *TelemostTransport) getConnectionInfo() error {
	client := &http.Client{Timeout: 15 * time.Second}
	var apiURL, method string
	if t.conferenceURL != "" {
		encodedURL := url.QueryEscape(t.conferenceURL)
		apiURL = fmt.Sprintf("https://cloud-api.yandex.ru/telemost_front/v2/telemost/conferences/%s/connection?next_gen_media_platform_allowed=true&waiting_room_supported=true", encodedURL)
		method = "GET"
	} else {
		apiURL = "https://cloud-api.yandex.ru/telemost_front/v2/telemost/conferences?next_gen_media_platform_allowed=true"
		method = "POST"
	}
	var reqBody io.Reader
	if method == "POST" {
		reqBody = bytes.NewBufferString("{}")
	}
	req, err := http.NewRequest(method, apiURL, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.6")
	req.Header.Set("Client-Instance-Id", t.clientInstanceID)
	req.Header.Set("Connection", "keep-alive")
	if method == "POST" {
		req.Header.Set("Cookie", t.cookies)
		req.Header.Set("X-Uid", t.userID)
	}
	req.Header.Set("Origin", "https://telemost.yandex.ru")
	req.Header.Set("Referer", "https://telemost.yandex.ru/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("User-Agent", telemostUserAgent)
	req.Header.Set("X-Telemost-Client-Version", "211.2.0")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("idempotency-key", t.idempotencyKey)

	utils.Debugf("[Telemost] HTTP %s %s", method, apiURL)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	utils.Debugf("[Telemost] HTTP %d (%d bytes)", resp.StatusCode, len(bodyBytes))
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	var connResp TelemostConnectionResponse
	if err := json.Unmarshal(bodyBytes, &connResp); err != nil {
		return fmt.Errorf("parse connection response: %w", err)
	}
	t.roomID = connResp.RoomID
	t.peerID = connResp.PeerID
	t.credentials = connResp.Credentials
	t.sessionID = connResp.SessionID
	t.peerSessionID = connResp.PeerSessionID
	if connResp.URI != "" {
		t.conferenceURL = connResp.URI
	}
	if ms, ok := connResp.ClientConfiguration["media_server_url"].(string); ok {
		t.mediaServerURL = ms
	} else {
		return fmt.Errorf("no media_server_url in response")
	}
	t.participantID = t.peerID
	utils.Debugf("[Telemost] joined: room=%s peer=%s media=%s", t.roomID, t.peerID, t.mediaServerURL)
	return nil
}

func (t *TelemostTransport) connectWS(attempt int) {
	if !t.IsRunning() {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Debugf("[Telemost] panic in connectWS: %v", r)
			}
		}()
		headers := http.Header{}
		headers.Set("User-Agent", telemostUserAgent)
		headers.Set("Origin", "https://telemost.yandex.ru")
		utils.Debugf("[Telemost] WS dial -> %s", t.mediaServerURL)
		conn, resp, err := websocket.DefaultDialer.Dial(t.mediaServerURL, headers)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			utils.Debugf("[Telemost] WS dial failed (status %d): %v", status, err)
			t.scheduleReconnect(attempt)
			return
		}
		utils.Debugf("[Telemost] WS connected")
		t.connMu.Lock()
		t.conn = conn
		t.SetConnected(true)
		t.connMu.Unlock()
		if err := t.sendHello(); err != nil {
			utils.Debugf("[Telemost] sendHello: %v", err)
			_ = conn.Close()
			t.SetConnected(false)
			t.scheduleReconnect(attempt)
			return
		}
		connectedAt := time.Now()
		for t.IsRunning() {
			mt, message, err := conn.ReadMessage()
			if err != nil {
				utils.Debugf("[Telemost] read error: %v", err)
				t.SetConnected(false)
				_ = conn.Close()
				next := attempt
				if time.Since(connectedAt) > 15*time.Second {
					next = -1
				}
				t.scheduleReconnect(next)
				return
			}
			if mt == websocket.TextMessage {
				t.handleTextMessage(message)
			}
		}
	}()
}

func (t *TelemostTransport) scheduleReconnect(attempt int) {
	next := attempt + 1
	if !t.IsRunning() || next >= t.GetConfig().MaxReconnectAttempts {
		return
	}
	d := reconnectBackoff(next)
	utils.Debugf("[Telemost] reconnect in %v (attempt %d)", d, next)
	select {
	case <-time.After(d):
	case <-t.Done():
		return
	}
	if !t.IsRunning() {
		return
	}
	t.stats.Reconnects.Add(1)
	t.connectWS(next)
}

func (t *TelemostTransport) writeWSMessage(messageType int, data []byte) error {
	t.wsSendMu.Lock()
	defer t.wsSendMu.Unlock()
	t.connMu.RLock()
	conn := t.conn
	t.connMu.RUnlock()
	if conn == nil {
		return fmt.Errorf("no connection")
	}
	return conn.WriteMessage(messageType, data)
}

func (t *TelemostTransport) sendUpdatePublisherTrackDescription() error {
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"updatePublisherTrackDescription": map[string]interface{}{
			"publisherTrackDescriptions": []map[string]interface{}{
				t.publisherTrackDescription(),
			},
		},
	}
	data, _ := json.Marshal(msg)
	utils.Debugf("[Telemost] updatePublisherTrackDescription -> %s/%s [%s]",
		t.publishKind.trackKindLabel(), t.publishKind.sdpLabel(), roleStr(t.isExitNode))
	return t.writeWSMessage(websocket.TextMessage, data)
}

// publisherTrackDescription describes the one outbound video track.
//
// The real web client builds this two ways, and mixing them is what kept the
// share slot from activating. For a track carried over RTP it sends
//
//	{mid, transceiverMid, kind, priority, label, codecs: {}, groupId, description}
//
// with an EMPTY codecs map - the codec is in the SDP. Only for a track carried
// over a DataChannel does it send
//
//	{kind, label, priority, dcLabel, mid: "", codecs: {<pt>: cap}, groupId, description}
//
// We were sending a hybrid of the two: a real mid together with dcLabel and a
// populated codecs map, which matches neither shape.
func (t *TelemostTransport) publisherTrackDescription() map[string]interface{} {
	// RTPPublisherTrack: the track rides the video m-line.
	desc := map[string]interface{}{
		"mid":            t.shareMID,
		"transceiverMid": t.shareMID,
		"kind":           t.publishKind.trackKindLabel(),
		"priority":       0,
		"label":          t.publishKind.sdpLabel(),
		"codecs":         map[string]interface{}{},
		"groupId":        t.publishKind.groupID(),
		"description":    "",
	}
	if t.publishKind == kindCamera {
		// Camera is just a VIDEO track in group 1; there is no DISPLAY kind.
		desc["label"] = "Camera"
	}
	return desc
}

func (t *TelemostTransport) sendSDKCodecsInfo() error {
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"sdkCodecsInfo": map[string]interface{}{
			"vp8": map[string]interface{}{
				"supported": "CODEC_FEATURE_SUPPORTED",
				"hwDecode":  "CODEC_FEATURE_NOT_SUPPORTED",
				"hwEncode":  "CODEC_FEATURE_NOT_SUPPORTED",
				"isoString": "vp8",
			},
			"vp9": []map[string]interface{}{
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_NOT_SUPPORTED", "isoString": "vp09.00.51.08"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_NOT_SUPPORTED", "isoString": "vp09.02.51.10"},
				{"supported": "CODEC_FEATURE_NOT_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_NOT_SUPPORTED", "isoString": "vp09.02.51.12"},
			},
			"av1": []map[string]interface{}{
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_NOT_SUPPORTED", "isoString": "av01.0.04M.08"},
				{"supported": "CODEC_FEATURE_NOT_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_NOT_SUPPORTED", "isoString": "av01.0.04M.10"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_NOT_SUPPORTED", "isoString": "av01.0.05M.08"},
				{"supported": "CODEC_FEATURE_NOT_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_NOT_SUPPORTED", "isoString": "av01.0.05M.10"},
			},
			"h264": []map[string]interface{}{
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.42e01f"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.42001f"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.4d001f"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.640034"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.420034"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.42e034"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.4d0034"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.640c1f"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.640020"},
				{"supported": "CODEC_FEATURE_SUPPORTED", "hwDecode": "CODEC_FEATURE_SUPPORTED", "hwEncode": "CODEC_FEATURE_SUPPORTED", "isoString": "avc1.64001f"},
			},
		},
	}
	data, _ := json.Marshal(msg)
	utils.Debugf("[Telemost] sdkCodecsInfo ->")
	return t.writeWSMessage(websocket.TextMessage, data)
}

func (t *TelemostTransport) sendSetSlots() error {
	key := t.slotKey.Add(1)
	// The slot dimensions tell the SFU how large the subscriber will render each
	// stream. The SFU sizes the bitrate it forwards to the slot: small
	// (thumbnail) dimensions get a low-bitrate layer, so the default 608x342
	// slots cap the forwarded sharing stream at a few hundred Kbps regardless of
	// how much the publisher sends. TELEMOST_SLOT_W/H request a larger slot so
	// the SFU forwards the full sharing bitrate (up to its ~2 Mbps cap).
	slots := []map[string]interface{}{
		{"width": 608, "height": 342}, {"width": 608, "height": 342},
		{"width": 416, "height": 234}, {"width": 416, "height": 234},
		{"width": 400, "height": 225}, {"width": 400, "height": 225},
		{"width": 288, "height": 162}, {"width": 288, "height": 162},
		{"width": 272, "height": 153}, {"width": 272, "height": 153},
		{"width": 272, "height": 153}, {"width": 272, "height": 153},
	}
	if ws := os.Getenv("TELEMOST_SLOT_W"); ws != "" {
		hs := os.Getenv("TELEMOST_SLOT_H")
		if w, err := strconv.Atoi(ws); err == nil && w > 0 {
			h, _ := strconv.Atoi(hs)
			if h <= 0 {
				h = w * 9 / 16
			}
			for i := range slots {
				slots[i] = map[string]interface{}{"width": w, "height": h}
			}
			utils.Debugf("[Telemost] setSlots dimensions overridden to %dx%d [%s]", w, h, roleStr(t.isExitNode))
		}
	}
	if n := envIntDefault("TELEMOST_SLOT_COUNT", 0); n > 0 && n < len(slots) {
		slots = slots[:n]
	}
	shutdown := false
	if t.lane != nil {
		slots = slots[:1]
		t.lane.mu.Lock()
		shutdown = t.lane.target == ""
		t.lane.shutdown = shutdown
		t.lane.mu.Unlock()
	}
	reqMsg := map[string]interface{}{
		"uid": uuid.New().String(),
		"setSlots": map[string]interface{}{
			"slots":              slots,
			"audioSlotsCount":    0,
			"key":                key,
			"shutdownAllVideo":   shutdown,
			"withSelfView":       envOrDefault("TELEMOST_SELF_VIEW", "1") != "0",
			"selfViewVisibility": "ON_LOADING_THEN_SHOW",
			"gridConfig":         map[string]interface{}{},
		},
	}
	data, _ := json.Marshal(reqMsg)
	utils.Debugf("[Telemost] setSlots -> key=%d slots=%d", key, len(slots))
	if err := t.writeWSMessage(websocket.TextMessage, data); err != nil {
		return err
	}
	if v := os.Getenv("TELEMOST_SLOT_OFFSET"); v != "" {
		if off, err := strconv.Atoi(v); err == nil {
			return t.sendSetSlotsOffset(off)
		}
	}
	return nil
}

// sendSetSlotsOffset pages the controller's join-order layout, like the web
// client's setSlotsOffset: slot 0 then shows the participant at that offset.
func (t *TelemostTransport) sendSetSlotsOffset(off int) error {
	data, _ := json.Marshal(map[string]interface{}{
		"uid":            uuid.New().String(),
		"setSlotsOffset": map[string]interface{}{"offset": off},
	})
	utils.Debugf("[Telemost] setSlotsOffset -> %d [%s]", off, roleStr(t.isExitNode))
	return t.writeWSMessage(websocket.TextMessage, data)
}

// slotsKeepaliveLoop re-sends setSlots every `sec` seconds. The SFU stops
// forwarding a subscribed sharing stream after ~28-30s of a long-lived stream
// unless the subscriber refreshes its slot interest; without this a high-rate
// tunnel stream freezes mid-run (delivery hard-stops, PC stays connected, no
// RTCP error). Gated by TELEMOST_SLOTS_KEEPALIVE_SEC (0 = off).
func (t *TelemostTransport) slotsKeepaliveLoop(sec int) {
	ticker := time.NewTicker(time.Duration(sec) * time.Second)
	defer ticker.Stop()
	utils.Debugf("[Telemost] slots keepalive every %ds [%s]", sec, roleStr(t.isExitNode))
	for t.IsRunning() {
		<-ticker.C
		if err := t.sendSetSlots(); err != nil {
			utils.Debugf("[Telemost] slots keepalive stopped: %v [%s]", err, roleStr(t.isExitNode))
			return
		}
	}
}

// pubDescKeepaliveLoop re-sends the publisher track description every `sec`
// seconds, in case the SFU reclaims the share from the publisher side rather
// than the subscriber side. Gated by TELEMOST_PUBDESC_KEEPALIVE_SEC (0 = off).
func (t *TelemostTransport) pubDescKeepaliveLoop(sec int) {
	ticker := time.NewTicker(time.Duration(sec) * time.Second)
	defer ticker.Stop()
	utils.Debugf("[Telemost] pubdesc keepalive every %ds [%s]", sec, roleStr(t.isExitNode))
	for t.IsRunning() {
		<-ticker.C
		if err := t.sendUpdatePublisherTrackDescription(); err != nil {
			utils.Debugf("[Telemost] pubdesc keepalive stopped: %v [%s]", err, roleStr(t.isExitNode))
			return
		}
	}
}

func (t *TelemostTransport) sendHello() error {
	msg := TelemostHello{
		UID: uuid.New().String(),
		Hello: TelemostHelloData{
			RoomID:        t.roomID,
			ParticipantID: t.participantID,
			Credentials:   t.credentials,
			ServiceName:   "telemost",
			// Hardcoded false here because video was always the carrier
			// until shareCarrier existed: a participant who tells the SFU
			// up front it will never send audio is a reasonable thing for
			// the SFU to just not bother forwarding, independent of
			// whatever actually turns up on the audio m-line afterward.
			SendAudio: t.shareCarrier == "audio",
			SendVideo: true,
			// The video track we publish is a DISPLAY_VIDEO (screen share)
			// track, and the SFU gates sharing slots on this flag. With it false
			// the SFU accepted the m-line, bound the SSRC, and still never
			// created a video slot - videoSlots stayed [].
			SendSharing: true,
			ParticipantAttributes: map[string]interface{}{
				"name": "OpenFlux", "role": "SPEAKER", "description": "",
			},
			ParticipantMeta: map[string]interface{}{
				"name":        "Гость",
				"role":        "SPEAKER",
				"description": "",
				"sendAudio":   t.shareCarrier == "audio",
				"sendVideo":   true,
				"sendSharing": true,
			},
			SDKInfo: map[string]interface{}{
				"implementation": "browser",
				"version":        "6.2.1",
				"userAgent":      telemostUserAgent,
				"hwConcurrency":  10,
			},
			SDKInitializationID: uuid.New().String(),
			CapabilitiesOffer: map[string]interface{}{
				"offerAnswerMode":                       []string{"SEPARATE"},
				"initialSubscriberOffer":                []string{"ON_HELLO"},
				"slotsMode":                             []string{"FROM_CONTROLLER"},
				"simulcastMode":                         []string{"DISABLED", "STATIC"},
				"selfVadStatus":                         []string{"FROM_SERVER", "FROM_CLIENT"},
				"dataChannelSharing":                    []string{"TO_RTP"},
				"videoEncoderConfig":                    []string{"NO_CONFIG", "ONLY_INIT_CONFIG", "RUNTIME_CONFIG"},
				"dataChannelVideoCodec":                 []string{"VP8", "UNIQUE_CODEC_FROM_TRACK_DESCRIPTION"},
				"bandwidthLimitationReason":             []string{"BANDWIDTH_REASON_DISABLED", "BANDWIDTH_REASON_ENABLED"},
				"sdkDefaultDeviceManagement":            []string{"SDK_DEFAULT_DEVICE_MANAGEMENT_DISABLED", "SDK_DEFAULT_DEVICE_MANAGEMENT_ENABLED"},
				"joinOrderLayout":                       []string{"JOIN_ORDER_LAYOUT_DISABLED", "JOIN_ORDER_LAYOUT_ENABLED"},
				"pinLayout":                             []string{"PIN_LAYOUT_DISABLED"},
				"sendSelfViewVideoSlot":                 []string{"SEND_SELF_VIEW_VIDEO_SLOT_DISABLED", "SEND_SELF_VIEW_VIDEO_SLOT_ENABLED"},
				"serverLayoutTransition":                []string{"SERVER_LAYOUT_TRANSITION_DISABLED"},
				"sdkPublisherOptimizeBitrate":           []string{"SDK_PUBLISHER_OPTIMIZE_BITRATE_DISABLED", "SDK_PUBLISHER_OPTIMIZE_BITRATE_FULL", "SDK_PUBLISHER_OPTIMIZE_BITRATE_ONLY_SELF"},
				"sdkNetworkLostDetection":               []string{"SDK_NETWORK_LOST_DETECTION_DISABLED"},
				"sdkNetworkPathMonitor":                 []string{"SDK_NETWORK_PATH_MONITOR_DISABLED"},
				"publisherVp9":                          []string{"PUBLISH_VP9_DISABLED", "PUBLISH_VP9_ENABLED"},
				"svcMode":                               []string{"SVC_MODE_DISABLED", "SVC_MODE_L3T3", "SVC_MODE_L3T3_KEY"},
				"subscriberOfferAsyncAck":               []string{"SUBSCRIBER_OFFER_ASYNC_ACK_DISABLED", "SUBSCRIBER_OFFER_ASYNC_ACK_ENABLED"},
				"androidBluetoothRoutingFix":            []string{"ANDROID_BLUETOOTH_ROUTING_FIX_DISABLED"},
				"fixedIceCandidatesPoolSize":            []string{"FIXED_ICE_CANDIDATES_POOL_SIZE_DISABLED"},
				"sdkAndroidTelecomIntegration":          []string{"SDK_ANDROID_TELECOM_INTEGRATION_DISABLED"},
				"setActiveCodecsMode":                   []string{"SET_ACTIVE_CODECS_MODE_DISABLED", "SET_ACTIVE_CODECS_MODE_VIDEO_ONLY"},
				"subscriberDtlsPassiveMode":             []string{"SUBSCRIBER_DTLS_PASSIVE_MODE_DISABLED", "SUBSCRIBER_DTLS_PASSIVE_MODE_ENABLED"},
				"publisherOpusDred":                     []string{"PUBLISHER_OPUS_DRED_DISABLED"},
				"publisherOpusLowBitrate":               []string{"PUBLISHER_OPUS_LOW_BITRATE_DISABLED"},
				"sdkAndroidDestroySessionOnTaskRemoved": []string{"SDK_ANDROID_DESTROY_SESSION_ON_TASK_REMOVED_DISABLED"},
				"publisherOpusDredAndroid":              []string{"PUBLISHER_OPUS_DRED_ANDROID_DISABLED"},
				"publisherOpusDredIos":                  []string{"PUBLISHER_OPUS_DRED_IOS_DISABLED"},
				"subscriberOpusDredAndroid":             []string{"SUBSCRIBER_OPUS_DRED_ANDROID_DISABLED"},
				"subscriberOpusDredIos":                 []string{"SUBSCRIBER_OPUS_DRED_IOS_DISABLED"},
				"svcModes":                              []string{"FALSE"},
				"reportTelemetryModes":                  []string{"TRUE"},
				"keepDefaultDevicesModes":               []string{"FALSE"},
			},
		},
	}
	data, _ := json.Marshal(msg)
	utils.Debugf("[Telemost] hello -> room=%s participant=%s", t.roomID, t.participantID)
	return t.writeWSMessage(websocket.TextMessage, data)
}

func (t *TelemostTransport) sendUpdateMe() error {
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"updateMe": map[string]interface{}{
			"participantMeta": map[string]interface{}{
				"name":        "OpenFlux",
				"role":        "SPEAKER",
				"description": "",
				"sendAudio":   t.shareCarrier == "audio",
				"sendVideo":   t.publishKind.sendsVideo(),
				"sendSharing": t.publishKind.sendsSharing(),
			},
			"participantAttributes": map[string]interface{}{
				"name":        "OpenFlux",
				"role":        "SPEAKER",
				"description": "",
			},
			"sendAudio":   t.shareCarrier == "audio",
			"sendVideo":   t.publishKind.sendsVideo(),
			"sendSharing": t.publishKind.sendsSharing(),
		},
	}
	data, _ := json.Marshal(msg)
	utils.Debugf("[Telemost] updateMe -> sendAudio=%v sendVideo=%v sendSharing=%v [%s]",
		t.shareCarrier == "audio", t.publishKind.sendsVideo(), t.publishKind.sendsSharing(), roleStr(t.isExitNode))
	return t.writeWSMessage(websocket.TextMessage, data)
}

func (t *TelemostTransport) sendAck(uid string) error {
	msg := map[string]interface{}{
		"uid": uid,
		"ack": map[string]interface{}{"status": map[string]interface{}{"code": "OK"}},
	}
	data, _ := json.Marshal(msg)
	utils.Debugf("[Telemost] ack -> uid=%s", uid)
	return t.writeWSMessage(websocket.TextMessage, data)
}

func (t *TelemostTransport) handleTextMessage(data []byte) {
	var msg map[string]interface{}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	uid, hasUID := msg["uid"].(string)

	// Inventory every inbound message before dispatch.
	//
	// The handler below recognises a fixed set of types and silently acks
	// everything else, so any message carrying ICE candidates under a name
	// nobody handled never appeared in a single log line. Counting messages by
	// their recognised types is what made the SFU look like it never sends
	// candidates - a conclusion drawn from a dispatcher, not from the wire.
	if utils.Level() >= utils.LevelDebug {
		utils.Debugf("[Telemost] WS <- keys: %s [%s]",
			strings.Join(sortedMapKeys(msg), ","), roleStr(t.isExitNode))
	}
	for path, val := range findCandidateValues(msg, "", 0) {
		utils.Debugf("[Telemost] WS <- candidate at %s: %s [%s]",
			path, val, roleStr(t.isExitNode))
	}

	if _, ok := msg["ack"]; ok {
		return
	}
	for _, k := range []string{"selfQualityReport", "vadActivity", "slotsMeta"} {
		if v, ok := msg[k]; ok {
			d, _ := json.Marshal(v)
			utils.Debugf("[Telemost] WS <- %s: %s [%s]", k, string(d), roleStr(t.isExitNode))
		}
	}
	if _, ok := msg["pong"]; ok {
		return
	}

	if sh, ok := msg["serverHello"].(map[string]interface{}); ok {
		t.applyServerHello(sh)
		if hasUID {
			t.sendAck(uid)
		}
		t.sendUpdateMe()
		if t.publishKind.publishes() {
			if err := t.initPublisherPC(); err != nil {
				utils.Debugf("[Telemost] initPublisherPC: %v", err)
			}
		} else {
			utils.Debugf("[Telemost] publisher disabled (TELEMOST_PUBLISH_KIND=%s), subscriber only [%s]",
				envOrDefault("TELEMOST_PUBLISH_KIND", "none"), roleStr(t.isExitNode))
		}
		// Автоматически отправляем начальные пакеты, не дожидаясь ответов.
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = t.sendUpdatePublisherTrackDescription()
			time.Sleep(50 * time.Millisecond)
			_ = t.sendSetSlots()
			time.Sleep(50 * time.Millisecond)
			_ = t.sendSDKCodecsInfo()
		}()
		// Keepalives to defeat the SFU's ~28-30s share-forward reclaim on
		// long-lived streams (see slotsKeepaliveLoop). Env-gated, 0 = off.
		if envOrDefault("TELEMOST_TELEMETRY", "1") != "0" {
			iv := 10 * time.Second
			if tc, ok := sh["telemetryConfiguration"].(map[string]interface{}); ok {
				if ms, ok := tc["sendingInterval"].(float64); ok && ms > 0 {
					iv = time.Duration(ms) * time.Millisecond
				}
			}
			utils.SafeGo("telemost.telemetry", func() { t.telemetryLoop(iv) })
		}
		if v := os.Getenv("TELEMOST_SLOTS_KEEPALIVE_SEC"); v != "" {
			if sec, _ := strconv.Atoi(v); sec > 0 {
				utils.SafeGo("telemost.slots-keepalive", func() { t.slotsKeepaliveLoop(sec) })
			}
		}
		if v := os.Getenv("TELEMOST_PUBDESC_KEEPALIVE_SEC"); v != "" {
			if sec, _ := strconv.Atoi(v); sec > 0 {
				utils.SafeGo("telemost.pubdesc-keepalive", func() { t.pubDescKeepaliveLoop(sec) })
			}
		}
		if err := t.initWebRTC(); err != nil {
			utils.Debugf("[Telemost] initWebRTC: %v", err)
		}
		return
	}
	if so, ok := msg["subscriberSdpOffer"].(map[string]interface{}); ok {
		utils.Debugf("[Telemost] WS <- subscriberSdpOffer")
		if hasUID {
			t.sendAck(uid)
		}
		t.handleSubscriberOffer(so)
		return
	}
	if pa, ok := msg["publisherSdpAnswer"].(map[string]interface{}); ok {
		utils.Debugf("[Telemost] WS <- publisherSdpAnswer")
		if hasUID {
			t.sendAck(uid)
		}
		t.handlePublisherAnswer(pa)
		return
	}
	if ice, ok := msg["webrtcIceCandidate"].(map[string]interface{}); ok {
		if hasUID {
			t.sendAck(uid)
		}
		t.handleICECandidate(ice)
		return
	}
	if rd, ok := msg["removeDescription"]; ok {
		if utils.Level() >= utils.LevelDebug {
			d, _ := json.Marshal(rd)
			utils.Debugf("[Telemost] WS <- removeDescription: %s", string(d))
		}
		if hasUID {
			t.sendAck(uid)
		}
		t.onDescriptions(rd, true)
		return
	}
	if sc, ok := msg["slotsConfig"]; ok {
		if utils.Level() >= utils.LevelDebug {
			d, _ := json.Marshal(sc)
			utils.Debugf("[Telemost] WS <- slotsConfig: %s", string(d))
		}
		if hasUID {
			t.sendAck(uid)
		}
		t.onSlotsConfig(sc)
		return
	}
	if up, ok := msg["upsertDescription"]; ok {
		if utils.Level() >= utils.LevelDebug {
			d, _ := json.Marshal(up)
			utils.Debugf("[Telemost] WS <- upsertDescription: %s", string(d))
		}
		if hasUID {
			t.sendAck(uid)
		}
		t.onDescriptions(up, false)
		return
	}
	if up, ok := msg["updateDescription"]; ok {
		if utils.Level() >= utils.LevelDebug {
			d, _ := json.Marshal(up)
			utils.Debugf("[Telemost] WS <- updateDescription: %s", string(d))
		}
		if hasUID {
			t.sendAck(uid)
		}
		t.onDescriptions(up, false)
		return
	}
	if _, ok := msg["setSlots"]; ok {
		if hasUID {
			t.sendAck(uid)
		}
		return
	}
	if hasUID {
		t.sendAck(uid)
	}
}

// sortedMapKeys lists an inbound message's top-level fields in a stable order,
// so a diff between two runs shows a new field rather than a reshuffled log.
func sortedMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// findCandidateValues walks a decoded message and returns every string that
// looks like an ICE candidate, together with the path it was found at.
//
// The question this answers is narrow and important: does the far end send its
// candidates in signaling at all? Grepping the handler for a candidate type
// only finds candidates in the shapes the handler already knows, so an
// unrouted message reads as an absent one. Searching values is what separates
// "the SFU does not send candidates" from "we do not parse where it puts them".
func findCandidateValues(v interface{}, path string, depth int) map[string]string {
	out := map[string]string{}
	if depth > 6 {
		return out
	}
	switch t := v.(type) {
	case string:
		if strings.Contains(strings.ToLower(t), "candidate") && len(t) > 4 {
			out[path] = t
		}
	case map[string]interface{}:
		for k, sub := range t {
			p := path + "." + k
			if path == "" {
				p = k
			}
			for sp, val := range findCandidateValues(sub, p, depth+1) {
				out[sp] = val
			}
		}
	case []interface{}:
		for i, sub := range t {
			for sp, val := range findCandidateValues(sub, fmt.Sprintf("%s[%d]", path, i), depth+1) {
				out[sp] = val
			}
		}
	}
	return out
}

func (t *TelemostTransport) applyServerHello(sh map[string]interface{}) {
	// The server states its protocol mode in serverHello: capabilitiesAnswer
	// says whether sharing rides a DataChannel or plain RTP, slotsMode says
	// who owns the layout, dataChannelVideoCodec and setActiveCodecs decide
	// which codec the publisher must negotiate. None of it is obvious from
	// the SDP, and guessing wrong is why the share slot never activated.
	if b, err := json.Marshal(sh); err == nil {
		utils.Debugf("[Telemost] serverHello raw: %s [%s]", string(b), roleStr(t.isExitNode))
	}
	for _, k := range []string{"capabilitiesAnswer", "capabilities", "configurations",
		"activeCodecs", "setActiveCodecs", "codecsConfiguration", "slotsMeta", "dataChannelSharing"} {
		if v, ok := sh[k]; ok {
			utils.Debugf("[Telemost] serverHello.%s = %v [%s]", k, v, roleStr(t.isExitNode))
		}
	}
	if v, ok := sh["sessionSecret"].(string); ok {
		t.serverConfig.sessionSecret = v
	}
	if v, ok := sh["sfuPeerInitializationId"].(string); ok {
		t.serverConfig.sfuPeerInitializationID = v
	}
	if pp, ok := sh["pingPongConfiguration"].(map[string]interface{}); ok {
		if v, ok := pp["pingInterval"].(float64); ok {
			t.serverConfig.pingInterval = int(v)
			// keepAliveLoop reads this live. The server states the cadence it
			// expects here; it's what actually governs the heartbeat, not the
			// hardcoded fallback keepAliveLoop starts with before this arrives.
			t.pingIntervalMs.Store(int64(v))
		}
		if v, ok := pp["ackTimeout"].(float64); ok {
			t.serverConfig.ackTimeout = int(v)
		}
	}
	if rtc, ok := sh["rtcConfiguration"].(map[string]interface{}); ok {
		t.serverConfig.rtcConfiguration = rtc
		if raw, ok := rtc["iceServers"].([]interface{}); ok {
			var out []webrtc.ICEServer
			for _, s := range raw {
				m, ok := s.(map[string]interface{})
				if !ok {
					continue
				}
				var urls []string
				switch u := m["urls"].(type) {
				case []interface{}:
					for _, x := range u {
						if str, ok := x.(string); ok {
							urls = append(urls, str)
						}
					}
				case string:
					urls = []string{u}
				}
				if len(urls) == 0 {
					continue
				}
				username, _ := m["username"].(string)
				cred, _ := m["credential"].(string)
				out = append(out, webrtc.ICEServer{
					URLs: urls, Username: username, Credential: cred,
				})
			}
			t.serverConfig.iceServers = out
		}
	}
	utils.Debugf("[Telemost] serverHello: iceServers=%d", len(t.serverConfig.iceServers))
}

// ---- PeerConnections ----

// newTelemostMediaEngine builds the MediaEngine the publisher uses.
//
// Besides the default codecs it registers the RTP header extensions a Chrome
// publisher always offers. Without at least the sdes:rtp-stream-id one, Pion
// emits no a=extmap lines at all (getRTPParametersByKind only walks
// MediaEngine.headerExtensions), so the offer never told the SFU that a
// simulcast SSRC could be learned from the RID extension.
// registerTelemostCodecs registers pion's default audio and video codecs
// without the RTX repair streams that pion v4 adds to them. This transport
// answers loss itself (nackRTX resends the original packet on its original
// SSRC/sequence number), and an RTX codec would make the offer declare a second
// SSRC per video track, which the SDP rewriting here does not expect.
func registerTelemostCodecs(m *webrtc.MediaEngine) error {
	for _, c := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: 111},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeG722, ClockRate: 8000}, PayloadType: 9},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000}, PayloadType: 0},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000}, PayloadType: 8},
	} {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}
	fb := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	video := func(mime, fmtp string, pt webrtc.PayloadType) webrtc.RTPCodecParameters {
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000, SDPFmtpLine: fmtp, RTCPFeedback: fb},
			PayloadType:        pt,
		}
	}
	const h264 = "level-asymmetry-allowed=1;packetization-mode=%d;profile-level-id=%s"
	for _, c := range []webrtc.RTPCodecParameters{
		video(webrtc.MimeTypeVP8, "", 96),
		video(webrtc.MimeTypeH264, fmt.Sprintf(h264, 1, "42001f"), 102),
		video(webrtc.MimeTypeH264, fmt.Sprintf(h264, 0, "42001f"), 104),
		video(webrtc.MimeTypeH264, fmt.Sprintf(h264, 1, "42e01f"), 106),
		video(webrtc.MimeTypeH264, fmt.Sprintf(h264, 0, "42e01f"), 108),
		video(webrtc.MimeTypeH264, fmt.Sprintf(h264, 1, "4d001f"), 127),
		video(webrtc.MimeTypeH264, fmt.Sprintf(h264, 0, "4d001f"), 39),
		video(webrtc.MimeTypeAV1, "", 45),
		video(webrtc.MimeTypeVP9, "profile-id=0", 98),
		video(webrtc.MimeTypeVP9, "profile-id=2", 100),
		video(webrtc.MimeTypeH264, fmt.Sprintf(h264, 1, "64001f"), 112),
	} {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	return nil
}

func newTelemostMediaEngine() (*webrtc.MediaEngine, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := registerTelemostCodecs(mediaEngine); err != nil {
		return nil, err
	}
	for _, uri := range []string{extURIRID, extURIRepairedRID, extURIMID, extURITWCC, extURIABS} {
		if err := mediaEngine.RegisterHeaderExtension(
			webrtc.RTPHeaderExtensionCapability{URI: uri},
			webrtc.RTPCodecTypeVideo,
		); err != nil {
			// Not fatal; only RID changes behaviour.
			utils.Debugf("[Telemost] RegisterHeaderExtension(%s): %v", uri, err)
		}
	}
	return mediaEngine, nil
}

// newTelemostSubscriberAPI builds the WebRTC API for the subscriber
// PeerConnection with receiver-side congestion feedback wired up.
//
// The SFU forwards a subscribed stream only as fast as its bandwidth estimate
// for that subscriber, and that estimate is driven entirely by the RTCP the
// subscriber sends back. The old subscriber used a bare MediaEngine with no
// interceptor registry, so it sent no transport-cc feedback and no receiver
// reports at all: the SFU's estimate never left the ~270 Kbps WebRTC start
// floor (serverHello: bandwidthLimitationReason ENABLED, no ramp over 60s) —
// that floor, not the pacing clock, was the real throughput cap.
//
// Registering transport-cc (so inbound RTP carries the transport-wide sequence
// number) plus the TWCC feedback interceptor and RTCP reports makes the
// subscriber report its (fat, loss-free) downlink honestly, letting the SFU's
// send-side estimator ramp the forward bitrate toward its declared per-stream
// cap (videoLayersConfiguration l1 = 1 Mbps, 4K sharing = 2 Mbps). Gated by
// TELEMOST_SUB_BWE (default off: with the REMB loop alone the SFU opens to
// the full ~16 Mbit/s per stream, while TWCC loss feedback collapsed its
// estimate under overload; =1 adds TWCC). TELEMOST_SUB_NACK=1 additionally adds a NACK generator (off by default:
// retransmits compete for the same forward byte-budget).
func newTelemostSubscriberAPI() (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := registerTelemostCodecs(mediaEngine); err != nil {
		return nil, err
	}
	if envOrDefault("TELEMOST_SUB_BWE", "0") == "0" {
		utils.Debugf("[Telemost] subscriber BWE feedback DISABLED (baseline)")
		return webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)), nil
	}
	// Register ONLY transport-cc: TWCC feedback needs the transport-wide
	// sequence number on inbound RTP. Deliberately NOT MID or RID — pion
	// demuxes incoming RTP by the MID/RID extensions when they are registered,
	// and the SFU's forwarded share video does not carry a MID that matches our
	// recvonly transceivers, so registering MID silently drops every video
	// packet before OnTrack (audio still arrives). SSRC-based demux (the
	// default when MID is absent) is what actually works here. As the answerer
	// we only emit transport-cc if the SFU offered it, so this is safe
	// regardless of what the SFU supports.
	useTWCC := envOrDefault("TELEMOST_SUB_TWCC", "1") != "0"
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if !useTWCC {
			break
		}
		if err := mediaEngine.RegisterHeaderExtension(
			webrtc.RTPHeaderExtensionCapability{URI: extURITWCC}, kind); err != nil {
			utils.Debugf("[Telemost] subscriber RegisterHeaderExtension(%s): %v", extURITWCC, err)
		}
	}
	ir := &interceptor.Registry{}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return nil, err
	}
	if envOrDefault("TELEMOST_SUB_NACK", "0") == "1" {
		if err := webrtc.ConfigureNack(mediaEngine, ir); err != nil {
			return nil, err
		}
	}
	if useTWCC {
		twccFb, err := twcc.NewSenderInterceptor()
		if err != nil {
			return nil, err
		}
		ir.Add(twccFb)
	}
	utils.Debugf("[Telemost] subscriber BWE feedback ENABLED (TWCC+RR, nack=%s)",
		envOrDefault("TELEMOST_SUB_NACK", "0"))
	return webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(ir)), nil
}

func (t *TelemostTransport) initPublisherPC() error {
	iceServers := t.serverConfig.iceServers
	if len(iceServers) == 0 {
		iceServers = []webrtc.ICEServer{{URLs: []string{"stun:stun.rtc.yandex.net:3478"}}}
	}
	config := webrtc.Configuration{
		ICEServers:   iceServers,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}
	// TELEMOST_ICE_POLICY=relay forces the TURN server to carry the media.
	//
	// This exists to answer a question logs cannot: ICE silently prefers host
	// and server-reflexive candidates, so a session that works proves nothing
	// about whether the advertised TURN server is usable at all. Relay-only
	// removes every other option, so either the tunnel runs over TURN or it
	// does not run.
	if envOrDefault("TELEMOST_ICE_POLICY", "all") == "relay" {
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
		utils.Debugf("[Telemost] ICE policy: relay-only (TURN mandatory) [%s]", roleStr(t.isExitNode))
	}
	mediaEngine, err := newTelemostMediaEngine()
	if err != nil {
		return err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create publisher PC: %w", err)
	}
	t.publisherPC = pc
	t.pubSeq.Store(1)
	t.trackReady.Store(false)
	t.publishMode = parsePublishMode(os.Getenv("TELEMOST_PUBLISH"))

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			t.sendICECandidate(candidate, "PUBLISHER")
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		utils.Debugf("[Telemost] publisher PC state=%s [%s]", state, roleStr(t.isExitNode))
		switch state {
		case webrtc.PeerConnectionStateConnected:
			t.trackReady.Store(true)
			utils.Debugf("[Telemost] publisher ready: ssrc=%08x pt=%d ridExt=%d layers=%s [%s]",
				t.shareSSRC, t.sharePayloadT, t.shareRIDExtID, t.layerDesc(),
				roleStr(t.isExitNode))
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			t.trackReady.Store(false)
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		utils.Debugf("[Telemost] publisher ICE state=%s [%s]", state, roleStr(t.isExitNode))
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		utils.Debugf("[Telemost] publisher ICE gathering=%s [%s]", state, roleStr(t.isExitNode))
	})
	// Local candidates, logged as they are gathered.
	//
	// Nothing here recorded which candidate types ICE actually produced, which
	// made a relay-only run impossible to interpret: a session that carried
	// media proved nothing about whether TURN was involved, because host and
	// server-reflexive candidates would have carried it either way. The type
	// prefix here is the whole answer - relay means the TURN server is
	// transporting, srflx means the peer's STUN address was reachable directly.
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		utils.Debugf("[Telemost] publisher candidate: %s [%s]",
			candidate.String(), roleStr(t.isExitNode))
	})

	// Audio transceiver. With shareCarrier=="video" (the default) the SFU
	// answers it recvonly anyway, so it is left sendonly and never written
	// to, same as always. With shareCarrier=="audio" this is the actual
	// data path: a fake-Opus TelemostTrack (canvas_audio.go) replaces its
	// default track, same pattern as the video track below.
	audioTransceiver, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly},
	)
	if err != nil {
		return fmt.Errorf("add audio transceiver: %w", err)
	}
	if t.shareCarrier == "audio" {
		audioTrack := NewTelemostTrack(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 1},
			webrtc.RTPCodecTypeAudio,
			uuid.New().String(), uuid.New().String(),
		)
		if err := audioTransceiver.Sender().ReplaceTrack(audioTrack); err != nil {
			return fmt.Errorf("replace audio track: %w", err)
		}
		t.audioTrack = audioTrack
		utils.SafeGo("telemost.audio-keepalive", t.runAudioPublisherKeepalive)
	}

	// Video transceiver created sendonly, so Pion emits "a=sendonly" instead of
	// "a=sendrecv". A publisher never receives, and the screen-share content
	// attribute only belongs on a sendonly m-line - the previous code anchored
	// "a=content:speaker,main" on a line that Pion never produced.
	videoTrack := NewTelemostTrack(
		webrtc.RTPCodecCapability{MimeType: t.shareCodecMime, ClockRate: 90000},
		webrtc.RTPCodecTypeVideo,
		uuid.New().String(), uuid.New().String(),
	)
	videoTransceiver, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly},
	)
	if err != nil {
		return fmt.Errorf("add video transceiver: %w", err)
	}
	// Pion's own codec registration order (RegisterDefaultCodecs) puts VP8
	// first, so without this the offer lists VP8 before whatever
	// shareCodecMime actually wants, and an SFU that just takes the
	// offerer's first choice would negotiate VP8 regardless of what
	// TelemostTrack.Bind() is looking for. Forcing a single-entry
	// preference list makes the offer unambiguous - and makes the SFU's
	// answer, if it has to reject the m-line outright, say so clearly
	// instead of us silently ending up back on VP8.
	var publisherCodecPref *webrtc.RTPCodecCapability
	switch t.shareCodecMime {
	case webrtc.MimeTypeVP9:
		publisherCodecPref = &webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0",
		}
	case webrtc.MimeTypeH264:
		publisherCodecPref = &webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		}
	}
	if publisherCodecPref != nil {
		if err := videoTransceiver.SetCodecPreferences([]webrtc.RTPCodecParameters{
			{RTPCodecCapability: *publisherCodecPref},
		}); err != nil {
			return fmt.Errorf("set publisher codec preference: %w", err)
		}
	}
	if err := videoTransceiver.Sender().ReplaceTrack(videoTrack); err != nil {
		return fmt.Errorf("replace video track: %w", err)
	}
	t.sharingTrack = videoTrack

	// The track description we send declares "dcLabel":"sharing", which tells
	// the SFU this DISPLAY_VIDEO track is carried alongside a DataChannel of
	// that label. Declaring a label whose DataChannel does not exist leaves the
	// sharing track without its associated channel, and the SFU drops the whole
	// track. Create it so the label resolves. A camera track declares no
	// dcLabel, so it needs no channel.
	if t.publishKind == kindSharing {
		dc, err := pc.CreateDataChannel("sharing", nil)
		if err != nil {
			utils.Debugf("[Telemost] CreateDataChannel(sharing): %v", err)
		} else {
			t.sharingDC = dc
			dc.OnOpen(func() {
				utils.Debugf("[Telemost] sharing DataChannel open [%s]", roleStr(t.isExitNode))
			})
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				utils.Debugf("[Telemost] DC(sharing) <- %d bytes: %q [%s]",
					len(msg.Data), truncHex(msg.Data, 24), roleStr(t.isExitNode))
			})
			dc.OnClose(func() {
				utils.Debugf("[Telemost] sharing DataChannel closed [%s]", roleStr(t.isExitNode))
			})
			dc.OnError(func(err error) {
				utils.Debugf("[Telemost] sharing DataChannel error: %v", err)
			})
		}
	}

	// RTCP tap. Pion v3.3.6's GetStats() does not collect RTPSender/RTPReceiver
	// stats at all, so it cannot tell us whether the SFU is ingesting the
	// stream. RTCP can: the SFU's Receiver Report for our SSRC only exists if
	// it actually received the packets, and its NACK/PLI/FIR tell us it tried
	// to decode them.
	go func() {
		rtcpBuf := make([]byte, 1500)
		var lastSummary string
		for {
			n, _, err := videoTransceiver.Sender().Read(rtcpBuf)
			if err != nil {
				return
			}
			t.answerNACKs(rtcpBuf[:n])
			summary := t.summarizeRTCP(rtcpBuf[:n])
			if summary == "" || summary == lastSummary {
				continue
			}
			lastSummary = summary
			utils.Debugf("[Telemost] publisher RTCP <- %s [%s]", summary, roleStr(t.isExitNode))
		}
	}()

	// A screen share is a continuous stream, not an on-demand one. The SFU
	// accepts the track (it answers our SSRC with Receiver Reports) but only
	// promotes it to a forwarded video slot once it sees sustained traffic, so
	// keep the track warm at a fixed frame rate. Real tunnel bytes ride on top
	// of this via sendBatch; the filler frames are what make the stream look
	// alive between packets.
	go t.runPublisherKeepalive()

	// TELEMOST_DC_PROBE=1 answers a question the logs could not: does the SFU
	// forward DataChannel messages between participants, or only open the
	// channel and leave it one-way? Nothing ever wrote to the channel, so a
	// forwarded message and a silently dropped one were indistinguishable.
	if envOrDefault("TELEMOST_DC_PROBE", "") != "" {
		go t.runDataChannelProbe()
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	local := pc.LocalDescription()
	plan, err := planVideoTrack(local.SDP, t.publishMode)
	if err != nil {
		return fmt.Errorf("plan video track: %w", err)
	}
	t.publishPlan = plan
	// These two are what sendBatch used to read as zero.
	t.shareSSRC = plan.info.boundSSRC
	t.shareRIDExtID = plan.info.ridExtID
	sendSDP := plan.sdp

	videoMid := plan.info.videoMid
	if videoMid == "" {
		videoMid = "1"
	}
	t.shareMID = videoMid

	// The payload type a codec lands on is Pion's choice, not ours (it
	// depends on registration order and whatever else shares the m-line),
	// so it has to be read back out of the offer we just built rather than
	// assumed. 96 was always VP8's payload type by coincidence of that
	// being Pion's first-registered video codec; it is not VP9's.
	codecShortName := strings.TrimPrefix(t.shareCodecMime, "video/")
	codecPT, ptFound := sdpPayloadTypeFor(sendSDP, codecShortName)
	if !ptFound {
		utils.Debugf("[Telemost] no payload type found for %s in our own offer, falling back to 96 [%s]",
			codecShortName, roleStr(t.isExitNode))
		codecPT = 96
	}
	offerMsg := map[string]interface{}{
		"uid": uuid.New().String(),
		"publisherSdpOffer": map[string]interface{}{
			"pcSeq": t.pubSeq.Load(),
			"sdp":   sendSDP,
			"tracks": []map[string]interface{}{
				{
					"kind": "DISPLAY_VIDEO", "label": "Primary Monitor", "priority": 0,
					"dcLabel": "sharing",
					"mid":     videoMid, "groupId": 2, "description": "",
					"codecs": map[string]interface{}{
						strconv.Itoa(codecPT): map[string]interface{}{
							"channels": 0, "clockRate": 90000,
							"mimeType": t.shareCodecMime, "sdpFmtpLine": "",
						},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(offerMsg)
	utils.Debugf("[Telemost] publisherSdpOffer -> pcSeq=%d mode=%s mid=%s ssrc=%08x ridExt=%d sdpLen=%d [%s]",
		t.pubSeq.Load(), t.publishMode, videoMid, t.shareSSRC, t.shareRIDExtID,
		len(sendSDP), roleStr(t.isExitNode))
	logSDPLines(sendSDP, "offer")
	if utils.IsVerbose() {
		utils.Verbosef("[Telemost] publisher SDP offer:\n%s", sendSDP)
	}
	return t.writeWSMessage(websocket.TextMessage, data)
}

// logSDPLines prints just the lines that decide whether the SFU accepts the
// track: direction, msid, rid/simulcast, declared SSRCs, header extensions.
func logSDPLines(sdpStr, tag string) {
	if utils.Level() < utils.LevelDebug {
		return
	}
	for _, l := range strings.Split(sdpStr, "\r\n") {
		if strings.HasPrefix(l, "a=rid:") || strings.HasPrefix(l, "a=simulcast:") ||
			strings.HasPrefix(l, "a=ssrc-group:") || strings.HasPrefix(l, "a=ssrc:") ||
			strings.HasPrefix(l, "a=extmap:") || strings.HasPrefix(l, "a=content:") ||
			strings.HasPrefix(l, "a=msid:") || strings.HasPrefix(l, "a=mid:") ||
			// Candidate lines matter as much as the rest. Without them there is
			// no way to tell from the logs who the far end of a session
			// actually is: the SFU, or another participant. That distinction
			// decides whether media travels peer-to-peer or through a gateway,
			// and it was invisible until this was added.
			strings.HasPrefix(l, "a=candidate:") ||
			strings.HasPrefix(l, "o=-") ||
			l == "a=sendonly" || l == "a=sendrecv" || l == "a=recvonly" || l == "a=inactive" {
			utils.Debugf("[Telemost]   %s| %s", tag, l)
		}
	}
}

// layerDesc renders the SSRC/RID mapping for debug logs.
func (t *TelemostTransport) layerDesc() string {
	if t.publishPlan == nil {
		return "[]"
	}
	parts := make([]string, 0, len(simulcastRIDs))
	for _, l := range t.publishPlan.Layers() {
		rid := l.rid
		if rid == "" {
			rid = "none"
		}
		parts = append(parts, fmt.Sprintf("%08x/%s", l.ssrc, rid))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func (t *TelemostTransport) handlePublisherAnswer(ans map[string]interface{}) {
	if t.publisherPC == nil {
		return
	}
	sdpStr, ok := ans["sdp"].(string)
	if !ok {
		utils.Debugf("[Telemost] No SDP in publisherSdpAnswer")
		return
	}
	utils.Debugf("[Telemost] publisherSdpAnswer <- sdpLen=%d", len(sdpStr))
	logSDPLines(sdpStr, "answer")
	if utils.IsVerbose() {
		utils.Verbosef("[Telemost] publisher SDP answer:\n%s", sdpStr)
	}
	// SetRemoteDescription с ОРИГИНАЛЬНЫМ answer от SFU.
	// Pion сверяет с локальным SDP (без a=content), m-line-структура совпадает.
	if err := t.publisherPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: sdpStr,
	}); err != nil {
		utils.Debugf("[Telemost] publisher SetRemote: %v", err)
		return
	}

	// Bind() is where Pion finally tells us the SSRC and payload type it
	// matched. Take them from the track rather than from the SDP text: the SFU
	// may renumber, and the SDP we sent was hand-edited afterwards.
	go func() {
		if t.sharingTrack.WaitBound(5 * time.Second) {
			t.shareSSRC = uint32(t.sharingTrack.SSRC())
			t.sharePayloadT = uint8(t.sharingTrack.PayloadType())
			if t.publishPlan.adoptBoundSSRC(t.shareSSRC) {
				utils.Debugf("[Telemost] WARNING: bound ssrc %08x != ssrc declared in the offer; "+
					"the SFU is mapping a different stream than the one we transmit on [%s]",
					t.shareSSRC, roleStr(t.isExitNode))
			}
			utils.Debugf("[Telemost] publisher bound: ssrc=%08x pt=%d ridExt=%d mode=%s layers=%s [%s]",
				t.shareSSRC, t.sharePayloadT, t.shareRIDExtID, t.publishMode,
				t.layerDesc(), roleStr(t.isExitNode))
		} else {
			utils.Debugf("[Telemost] publisher track never bound [%s]", roleStr(t.isExitNode))
		}
		_ = t.sharingTrack.Probe()
		// A browser announces its CNAME in an RTCP SDES chunk and then keeps
		// sending Sender Reports. Pion emits neither for a custom TrackLocal,
		// so do it here - the SFU ingests our RTP (it answers with RR) but
		// never promotes the stream to a slot without them.
		go t.runPublisherRTCP()
		time.Sleep(150 * time.Millisecond)
		_ = t.sendUpdatePublisherTrackDescription()
		time.Sleep(200 * time.Millisecond)
		_ = t.sendSetSlots()
		time.Sleep(100 * time.Millisecond)
		_ = t.sendSDKCodecsInfo()
		// Pion's own counters are the only trustworthy answer to "did the SFU
		// ever see our video": if remote-inbound-rtp never appears, it is
		// sending no RTCP RR and therefore never received the stream.
		go t.logPublisherStats()
	}()
}

// runPublisherKeepalive emits one frame per tick so the published track is a
// continuous stream.
//
// The tunnel is bursty: sendBatch only runs when there is TCP traffic to
// forward, which on an idle link is a handful of packets per minute. An SFU
// that gates slot allocation on sustained inbound media never sees a stream
// in that pattern - it accepted our SSRC and answered with Receiver Reports,
// but videoSlots stayed empty and no subscriber ever got a track. Sending a
// steady frame rate makes the stream unconditionally live, which is also what
// a real screen share does.
//
// This loop is also the only writer. Tunnel data does not go out the instant it
// arrives; it is queued and emitted on the next tick, so every frame in the
// published stream is exactly one frame interval after the last. Writing data
// frames eagerly, as they arrived, left the published stream looking like 30fps
// filler with the occasional isolated data frame seconds away from anything
// else. The SFU ingested that stream - it answered with receiver reports - but
// withheld nearly every data frame from subscribers and released the survivors
// many seconds late, so the client saw the real SYN-ACK long after its own
// connect attempt had given up. Issuing data on the frame clock keeps the
// whole stream evenly spaced, which is the only shape the SFU's forwarding path
// has accepted so far.
// publishFPS is how often runPublisherKeepalive's clock ticks.
//
// Raised from the original 30 to directly cut the tunnel's own round-trip
// latency floor. Each hop a packet takes through this transport waits, on
// average, half a tick before the next publish tick carries it, and a
// virtual TCP connection riding on top of two such hops (data one way, the
// ACK back) has that baked into its own measured RTT. A window-limited TCP
// connection's throughput is bounded by window/RTT, and gVisor's own
// receive-window auto-tuning grows the window in proportion to the
// throughput it has actually observed - so a connection stuck believing its
// RTT is 100ms+ (30fps's ~33ms per hop, compounding across the handshake and
// every ACK after it) stays capped in a way no amount of extra sending
// budget fixes, because that budget was never the bottleneck: real traffic
// measured nowhere near saturating even the original 30fps*maxFramesPerTick
// ceiling. 120fps cuts the per-hop wait to under 9ms.
var publishFPS = envIntDefault("TELEMOST_FPS", 120)

// rtpTimestampStep is how much the RTP timestamp advances per frame, at the
// standard 90kHz video clock rate. It has to track publishFPS: advancing it
// by a step sized for a different rate would make the stream's timestamps
// run faster or slower than wall-clock time, which is exactly the kind of
// thing the SFU's own jitter and RTCP accounting watches for.
var rtpTimestampStep = uint32(90000 / publishFPS)

// publishPace spreads a tick's frames across the tick (default). TELEMOST_PACE=0
// writes them back to back.
var publishPace = envOrDefault("TELEMOST_PACE", "0") != "0"

// envIntDefault reads a positive integer tuning knob.
func envIntDefault(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// wallClockTS makes the share's RTP timestamp follow wall-clock time at
// 90kHz (default). The old fixed step per packet ran the media clock ~4x
// faster than real time once data filled every tick.
var wallClockTS = envOrDefault("TELEMOST_TS_MODE", "wall") == "wall"

var tsEpoch = time.Now()

// nextShareTS returns the RTP timestamp for the next share frame.
func (t *TelemostTransport) nextShareTS() uint32 {
	if !wallClockTS {
		return t.shareTS.Add(rtpTimestampStep)
	}
	want := uint32(time.Since(tsEpoch) * 90000 / time.Second)
	for {
		cur := t.shareTS.Load()
		next := want
		if int32(next-cur) <= 0 {
			next = cur + 1
		}
		if t.shareTS.CompareAndSwap(cur, next) {
			return next
		}
	}
}

func (t *TelemostTransport) runPublisherKeepalive() {
	if !t.sharingTrack.WaitBound(10 * time.Second) {
		return
	}
	if t.shareCodecMime == webrtc.MimeTypeH264 {
		if err := t.sendH264ParameterSets(); err != nil {
			utils.Debugf("[Telemost] sendH264ParameterSets: %v [%s]", err, roleStr(t.isExitNode))
		}
	}
	frameInterval := time.Second / time.Duration(publishFPS)
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	// The exit emits the self-identifying filler so the client can tell the
	// exit's stream apart from its own self-view echo.
	filler := keepalivePayload
	if t.isExitNode {
		filler = probeKeepalivePayload
	}
	sent := 0
	for range ticker.C {
		if !t.IsRunning() || !t.trackReady.Load() {
			continue
		}
		frames := t.popFrames()
		if len(frames) == 0 {
			frames = [][]byte{filler(sent)}
		}
		// Spread this tick's frames evenly across the tick instead of
		// writing them back to back. Fragments carry a frameSeq
		// (transport/yandex/fragment.go) precisely so the receiver can tell
		// two frames' fragments apart when they interleave, but it can only
		// reassemble one frame at a time - a second frame's fragment
		// arriving before the first one finishes still costs the first one.
		// Firing up to maxFramesPerTick frames in a single burst gave the
		// network far more chance to reorder across that many packets at
		// once than a real encoder's paced output ever would, and that
		// reordering is exactly what measurably happened ("frame
		// reassembly preempted" in the logs, tracking almost one for one
		// with stalled TCP retransmits upstream). Pacing keeps the
		// aggregate rate this tick unchanged while making that reordering,
		// and the costly stall-and-retransmit cycle it was triggering, far
		// less likely.
		gap := frameInterval / time.Duration(len(frames))
		for i, payload := range frames {
			if i > 0 && publishPace {
				time.Sleep(gap)
			}
			// Hand over the raw wire frame, not a VP8 frame: sendFrame does
			// the wrapping. Passing an already-wrapped frame in here wrapped
			// it twice, so the receiver saw an outer VP8 descriptor followed
			// by a whole second VP8 frame, and decodeBatch read that inner
			// frame's 0x90 payload descriptor as a batch version byte.
			if err := t.sendFrame(payload); err != nil {
				utils.Debugf("[Telemost] sendFrame: %v", err)
			}
		}
		sent++
		if sent%90 == 0 {
			utils.Debugf("[Telemost] publisher keepalive: %d frames, %d queued [%s]",
				sent, t.queuedFrames(), roleStr(t.isExitNode))
		}
	}
}

// keepalivePayload is the filler body for a frame that carries no tunnel data.
// It is a well-formed wire-v2 batch frame with zero records, so the receiving
// end decodes it to an empty packet list instead of logging a decode error
// thirty times a second. The sequence number keeps the frames from compressing
// against each other, so the VP8 partition size stays in the range the SFU is
// used to seeing.
func keepalivePayload(seq int) []byte {
	// The record area is a sequence of [length][packet] pairs, so the only
	// empty frame is one with no records at all - appending even a 0x0000
	// length prefix would be read as a zero-length packet followed by a
	// truncated one. Building this from the shared encoder rather than from
	// literal bytes keeps it from drifting away from the real wire format
	// again, which is exactly how the data path broke in the first place.
	_ = seq
	return transport.EncodeBatchFrame(nil)
}

// probeKeepalivePayload is a filler frame that decodes to exactly one
// zero-length record instead of none.
//
// Both nodes send 30fps filler, and the SFU sends each participant its own
// stream back as a self-view slot, so on the subscriber side there is no way to
// tell "the peer is publishing" from "I am hearing myself" - both arrive as
// small frames on the same track with an SSRC the SFU has rewritten. Giving one
// role a filler that decodes to a single empty packet makes the two
// distinguishable: a receiver that delivers a zero-length packet is receiving
// the other node, and one that never does is only hearing itself.
func probeKeepalivePayload(seq int) []byte {
	_ = seq
	return transport.EncodeBatchFrame([][]byte{{}})
}

// summarizeMediaDirections renders one line per m-section of an SDP as
// "mid=dir". It is the fastest way to see whether the SFU is offering to send
// video to us (sendonly) or asking us to send (recvonly) - forceRecvonlyVideo
// only makes sense in the first case, and answering recvonly to a recvonly
// offer leaves both sides silent, which is indistinguishable from "the SFU has
// nothing to send".
func summarizeMediaDirections(sdpStr string) string {
	var parts []string
	var mid, dir string
	flush := func() {
		if mid == "" {
			return
		}
		if dir == "" {
			dir = "(default=sendrecv)"
		}
		parts = append(parts, mid+"="+dir)
	}
	for _, line := range strings.Split(sdpStr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			flush()
			mid, dir = "", ""
		case strings.HasPrefix(line, "a=mid:"):
			mid = strings.TrimPrefix(line, "a=mid:")
		case strings.HasPrefix(line, "a=recvonly"),
			strings.HasPrefix(line, "a=sendonly"),
			strings.HasPrefix(line, "a=sendrecv"),
			strings.HasPrefix(line, "a=inactive"):
			dir = strings.TrimPrefix(line, "a=")
		}
	}
	flush()
	return strings.Join(parts, " ")
}

// runPublisherRTCP sends the RTCP a browser publisher always sends: an SDES
// chunk carrying the CNAME, then a Sender Report roughly once a second.
//
// The SFU already acknowledges our RTP (it returns Receiver Reports for our
// SSRC), so the media path itself works. What it needs before it will route
// the stream to subscribers is this RTCP: the SDES is how it learns the
// stream's canonical name, and the periodic SR is how it sees the stream as
// live rather than a one-off burst.
func (t *TelemostTransport) runPublisherRTCP() {
	track := t.sharingTrack
	pc := t.publisherPC
	if track == nil || pc == nil {
		return
	}
	if !track.WaitBound(5 * time.Second) {
		return
	}
	ssrc := uint32(track.SSRC())
	if ssrc == 0 {
		return
	}

	if err := pc.WriteRTCP([]rtcp.Packet{
		rtcp.NewCNAMESourceDescription(ssrc, track.CNAME()),
	}); err != nil {
		utils.Debugf("[Telemost] RTCP SDES: %v", err)
	} else {
		utils.Debugf("[Telemost] RTCP SDES -> ssrc=%08x cname=%s [%s]",
			ssrc, track.CNAME(), roleStr(t.isExitNode))
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !t.IsRunning() {
			return
		}
		sr := track.SenderReport()
		if sr == nil {
			continue
		}
		// An SR with a zero packet count carries no information and some SFUs
		// treat it as a malformed report.
		if sr.PacketCount == 0 {
			continue
		}
		if err := pc.WriteRTCP([]rtcp.Packet{sr}); err != nil {
			utils.Debugf("[Telemost] RTCP SR: %v", err)
			continue
		}
		if utils.Level() >= utils.LevelDebug {
			utils.Debugf("[Telemost] RTCP SR -> ssrc=%08x pkts=%d octets=%d [%s]",
				ssrc, sr.PacketCount, sr.OctetCount, roleStr(t.isExitNode))
		}
	}
}

// summarizeRTCP renders the interesting parts of an RTCP compound packet: the
// per-SSRC receiver reports (proof the remote actually ingests our stream) and
// any feedback messages that mean the remote is unhappy with what we sent.
// nackRTX answers the SFU's NACKs by resending the original packet (same
// SSRC and sequence number) from a ring of recently sent packets. Without it
// every loss the SFU asks about stays a hole forever. TELEMOST_NACK_RTX=0
// disables.
var nackRTX = envOrDefault("TELEMOST_NACK_RTX", "1") != "0"

const rtxRingSize = 4096

type rtxEntry struct {
	hdr     rtp.Header
	payload []byte
	ok      bool
}

func (t *TelemostTransport) rtxRemember(hdr rtp.Header, payload []byte) {
	t.rtxMu.Lock()
	t.rtxRing[hdr.SequenceNumber%rtxRingSize] = rtxEntry{hdr: hdr, payload: payload, ok: true}
	t.rtxMu.Unlock()
}

func (t *TelemostTransport) answerNACKs(b []byte) {
	pkts, err := rtcp.Unmarshal(b)
	if err != nil {
		return
	}
	for _, p := range pkts {
		if rr, ok := p.(*rtcp.ReceiverReport); ok {
			for _, rep := range rr.Reports {
				t.uplinkLoss.Store(uint32(rep.FractionLost))
				t.sfuGot.Store(uint64(rep.LastSequenceNumber) - uint64(rep.TotalLost))
			}
			continue
		}
		nack, ok := p.(*rtcp.TransportLayerNack)
		if !ok || !nackRTX {
			continue
		}
		var seqs []uint16
		for _, pair := range nack.Nacks {
			seqs = append(seqs, pair.PacketList()...)
		}
		for _, seq := range seqs {
			t.rtxStats.asked.Add(1)
			t.rtxMu.Lock()
			e := t.rtxRing[seq%rtxRingSize]
			t.rtxMu.Unlock()
			if !e.ok || e.hdr.SequenceNumber != seq {
				t.rtxStats.missed.Add(1)
				continue
			}
			if _, err := t.sharingTrack.WriteHeader(e.hdr, e.payload); err == nil {
				t.rtxStats.resent.Add(1)
			}
		}
		utils.Debugf("[Telemost] NACK seqs=%v -> rtx asked=%d resent=%d missed=%d [%s]", seqs,
			t.rtxStats.asked.Load(), t.rtxStats.resent.Load(), t.rtxStats.missed.Load(), roleStr(t.isExitNode))
	}
}

func (t *TelemostTransport) summarizeRTCP(b []byte) string {
	pkts, err := rtcp.Unmarshal(b)
	if err != nil {
		return ""
	}
	var parts []string
	for _, p := range pkts {
		switch v := p.(type) {
		case *rtcp.ReceiverReport:
			for _, r := range v.Reports {
				// LastSequenceNumber is the extended highest sequence number the
				// receiver saw, so it is the direct evidence of ingestion.
				parts = append(parts, fmt.Sprintf("RR ssrc=%08x fracLost=%d lost=%d lastSeq=%d jitter=%.1f",
					r.SSRC, r.FractionLost, r.TotalLost, r.LastSequenceNumber, float64(r.Jitter)/64))
			}
			if len(v.Reports) == 0 {
				parts = append(parts, "RR (no reports)")
			}
		case *rtcp.PictureLossIndication:
			parts = append(parts, fmt.Sprintf("PLI ssrc=%08x", uint32(v.MediaSSRC)))
		case *rtcp.FullIntraRequest:
			parts = append(parts, fmt.Sprintf("FIR ssrc=%08x", uint32(v.MediaSSRC)))
		case *rtcp.TransportLayerNack:
			parts = append(parts, fmt.Sprintf("NACK ssrc=%08x lost=%d", uint32(v.MediaSSRC), len(v.Nacks)))
		case *rtcp.SliceLossIndication:
			parts = append(parts, fmt.Sprintf("SLI ssrc=%08x", uint32(v.MediaSSRC)))
		}
	}
	return strings.Join(parts, " ")
}

// logPublisherStats dumps the publisher PC's RTP/RTCP counters once a second.
// Pion reports outbound-rtp (what we wrote) and remote-inbound-rtp (the SFU's
// RTCP RR); the second one is the signal that the SFU is actually ingesting
// the track rather than silently dropping it.
func (t *TelemostTransport) logPublisherStats() {
	for i := 0; i < 8; i++ {
		time.Sleep(time.Second)
		pc := t.publisherPC
		if pc == nil {
			return
		}
		report := pc.GetStats()
		var out []string
		for _, s := range report {
			switch st := s.(type) {
			case webrtc.OutboundRTPStreamStats:
				out = append(out, fmt.Sprintf("out(ssrc=%08x sent=%d bytes=%d track=%s)",
					uint32(st.SSRC), st.PacketsSent, st.BytesSent, st.TrackID))
			case webrtc.RemoteInboundRTPStreamStats:
				// The SFU's RTCP RR. Its presence is the proof that the SFU
				// is ingesting the track; packetsLost/RTT come from it too.
				out = append(out, fmt.Sprintf("rr(ssrc=%08x recv=%d lost=%d rtt=%.3f jitter=%.3f)",
					uint32(st.SSRC), st.PacketsReceived, st.PacketsLost, st.RoundTripTime, st.Jitter))
			case webrtc.InboundRTPStreamStats:
				out = append(out, fmt.Sprintf("in(ssrc=%08x recv=%d)", uint32(st.SSRC), st.PacketsReceived))
			case webrtc.DataChannelStats:
				out = append(out, fmt.Sprintf("dc(label=%s state=%s)", st.Label, st.State.String()))
			}
		}
		if len(out) > 0 {
			utils.Debugf("[Telemost] pubstats: %s [%s]", strings.Join(out, " "), roleStr(t.isExitNode))
		}
	}
}

func (t *TelemostTransport) initWebRTC() error {
	iceServers := t.serverConfig.iceServers
	if len(iceServers) == 0 {
		iceServers = []webrtc.ICEServer{{URLs: []string{"stun:stun.rtc.yandex.net:3478"}}}
	}
	config := webrtc.Configuration{
		ICEServers:   iceServers,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}
	// The policy has to be set here too, not only on the publisher. Setting it
	// on one PeerConnection leaves the others free to connect directly, so a
	// run that looks relay-only is really a mix: the publisher forced through
	// TURN while the subscriber went straight to the SFU. That mixture is why
	// such a run could still receive Receiver Reports while TURN reported 403.
	if envOrDefault("TELEMOST_ICE_POLICY", "all") == "relay" {
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
		utils.Debugf("[Telemost] subscriber ICE policy: relay-only [%s]", roleStr(t.isExitNode))
	}
	api, err := newTelemostSubscriberAPI()
	if err != nil {
		return err
	}
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create subscriber PC: %w", err)
	}
	// Явно добавляем recvonly transceiver'ы: 2 video + 2 audio, как в
	// subscriberSdpOffer от SFU.
	var videoTransceivers []*webrtc.RTPTransceiver
	for i := 0; i < 2; i++ {
		tr, err := pc.AddTransceiverFromKind(
			webrtc.RTPCodecTypeVideo,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
		)
		if err != nil {
			return fmt.Errorf("add video transceiver: %w", err)
		}
		videoTransceivers = append(videoTransceivers, tr)
	}

	// Prefer more efficient codecs: VP9 > H.264 High Profile > VP8.
	// The SFU's offer includes all of them; without this Pion picks the first
	// match (VP8), which caps quality. Setting preferences lets the answer
	// select a better codec if the SFU supports it.
	for _, tr := range videoTransceivers {
		if err := tr.SetCodecPreferences([]webrtc.RTPCodecParameters{
			{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:    webrtc.MimeTypeVP9,
					ClockRate:   90000,
					SDPFmtpLine: "profile-id=0",
				},
			},
			{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:    webrtc.MimeTypeH264,
					ClockRate:   90000,
					SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
				},
			},
			{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:  webrtc.MimeTypeVP8,
					ClockRate: 90000,
				},
			},
		}); err != nil {
			utils.Debugf("[Telemost] subscriber SetCodecPreferences: %v [%s]", err, roleStr(t.isExitNode))
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := pc.AddTransceiverFromKind(
			webrtc.RTPCodecTypeAudio,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
		); err != nil {
			return fmt.Errorf("add audio transceiver: %w", err)
		}
	}

	t.subscriberPC = pc
	t.subSeq.Store(1)
	t.sdpSemantics = webrtc.SDPSemanticsUnifiedPlan

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			t.sendICECandidate(candidate, "SUBSCRIBER")
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		utils.Debugf("[Telemost] subscriber PC state=%s [%s]", state, roleStr(t.isExitNode))
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		utils.Debugf("[Telemost] subscriber ICE state=%s [%s]", state, roleStr(t.isExitNode))
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		utils.Debugf("[Telemost] 🎥 remote track: kind=%s id=%s codec=%s [%s]",
			track.Kind(), track.ID(), track.Codec().MimeType, roleStr(t.isExitNode))
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			mime := track.Codec().MimeType
			if mime == webrtc.MimeTypeVP8 || mime == webrtc.MimeTypeVP9 || mime == webrtc.MimeTypeH264 {
				go t.readSharingVideoTrack(track)
			} else {
				utils.Debugf("[Telemost] 🎥 unsupported video codec %s [%s]", mime, roleStr(t.isExitNode))
			}
		} else if track.Kind() == webrtc.RTPCodecTypeAudio && t.shareCarrier == "audio" {
			go t.readAudioCarrierTrack(track)
		}
	})
	t.observeInboundDataChannel(pc)
	// The subscriber's candidates matter for the same reason the publisher's
	// do: whether an incoming track arrived over TURN or over a direct
	// connection is only visible from the candidate types, and the two look
	// identical in every other log line.
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		utils.Debugf("[Telemost] subscriber candidate: %s [%s]",
			candidate.String(), roleStr(t.isExitNode))
	})
	return nil
}

func detectSDPSemantics(sdpStr string) webrtc.SDPSemantics {
	mids := map[string]bool{}
	mediaTypes := map[string]int{}
	for _, line := range strings.Split(sdpStr, "\r\n") {
		if strings.HasPrefix(line, "m=") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				mediaTypes[strings.TrimPrefix(fields[0], "m=")]++
			}
		} else if strings.HasPrefix(line, "a=mid:") {
			mids[strings.TrimPrefix(line, "a=mid:")] = true
		}
	}
	hasMultipleSameType := false
	for _, c := range mediaTypes {
		if c > 1 {
			hasMultipleSameType = true
			break
		}
	}
	if hasMultipleSameType && len(mids) > 2 {
		return webrtc.SDPSemanticsUnifiedPlan
	}
	return webrtc.SDPSemanticsUnifiedPlan
}

func removeSSRCGroups(sdpStr string) string {
	sd := &sdp.SessionDescription{}
	if err := sd.Unmarshal([]byte(sdpStr)); err != nil {
		utils.Debugf("[Telemost] SDP parse failed: %v", err)
		return sdpStr
	}
	for i := range sd.MediaDescriptions {
		var filtered []sdp.Attribute
		seen := map[string]bool{}
		keep := ""
		for _, a := range sd.MediaDescriptions[i].Attributes {
			if a.Key == "ssrc" {
				parts := strings.Fields(a.Value)
				if len(parts) > 0 && keep == "" {
					keep = parts[0]
				}
			}
		}
		for _, a := range sd.MediaDescriptions[i].Attributes {
			if a.Key == "ssrc-group" {
				continue
			}
			if a.Key == "ssrc" {
				parts := strings.Fields(a.Value)
				if len(parts) > 0 && parts[0] != keep {
					continue
				}
				if seen[a.Value] {
					continue
				}
				seen[a.Value] = true
			}
			filtered = append(filtered, a)
		}
		sd.MediaDescriptions[i].Attributes = filtered
	}
	out, err := sd.Marshal()
	if err != nil {
		return sdpStr
	}
	return string(out)
}

func (t *TelemostTransport) handleSubscriberOffer(offer map[string]interface{}) {
	if t.subscriberPC == nil {
		if err := t.initWebRTC(); err != nil {
			utils.Debugf("[Telemost] initWebRTC: %v", err)
			return
		}
	}
	sdpStr, ok := offer["sdp"].(string)
	if !ok {
		return
	}
	pcSeq := 1
	if v, ok := offer["pcSeq"].(float64); ok {
		pcSeq = int(v)
	}
	t.subSeq.Store(int32(pcSeq))
	detected := detectSDPSemantics(sdpStr)
	mVideo := strings.Count(sdpStr, "m=video")
	mAudio := strings.Count(sdpStr, "m=audio")
	utils.Debugf("[Telemost] subscriberSdpOffer <- pcSeq=%d sdpLen=%d mVideo=%d mAudio=%d semantics=%v",
		pcSeq, len(sdpStr), mVideo, mAudio, detected)
	// The SFU's own candidate lines decide whether this session talks to a
	// gateway or straight to another participant, and this SDP was the only
	// place that answer could be read from - the offer was measured and counted
	// but never shown.
	logSDPLines(sdpStr, "suboffer")
	// logSDPLines filters, and the filter is what produced the false reading
	// that the SFU sends no candidates at all. The raw SDP is dumped so the
	// candidate lines can be read rather than inferred from their absence.
	if utils.Level() >= utils.LevelDebug {
		utils.Debugf("[Telemost] subscriber SDP offer (raw %d bytes):\n%s", len(sdpStr), sdpStr)
	}
	utils.Debugf("[Telemost] subscriber offer media: %s [%s]",
		summarizeMediaDirections(sdpStr), roleStr(t.isExitNode))

	if detected == webrtc.SDPSemanticsUnifiedPlan {
		sdpStr = removeSSRCGroups(sdpStr)
	}
	if t.sdpSemantics != detected && t.subscriberPC != nil {
		utils.Debugf("[Telemost] recreating subscriber PC with %v", detected)
		t.subscriberPC.Close()
		iceServers := t.serverConfig.iceServers
		if len(iceServers) == 0 {
			iceServers = []webrtc.ICEServer{{URLs: []string{"stun:stun.rtc.yandex.net:3478"}}}
		}
		config := webrtc.Configuration{ICEServers: iceServers, SDPSemantics: detected}
		if envOrDefault("TELEMOST_ICE_POLICY", "all") == "relay" {
			config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
		}
		api, err := newTelemostSubscriberAPI()
		if err != nil {
			utils.Debugf("[Telemost] recreate subscriber API: %v", err)
			return
		}
		pc, err := api.NewPeerConnection(config)
		if err != nil {
			utils.Debugf("[Telemost] recreate subscriber PC: %v", err)
			return
		}
		t.subscriberPC = pc
		t.sdpSemantics = detected
		pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			utils.Debugf("[Telemost] 🎥 remote track: kind=%s id=%s codec=%s [%s]",
				track.Kind(), track.ID(), track.Codec().MimeType, roleStr(t.isExitNode))
			if track.Kind() == webrtc.RTPCodecTypeVideo && track.Codec().MimeType == webrtc.MimeTypeVP8 {
				go t.readSharingVideoTrack(track)
			} else if track.Kind() == webrtc.RTPCodecTypeAudio && t.shareCarrier == "audio" {
				go t.readAudioCarrierTrack(track)
			}
		})
		t.observeInboundDataChannel(pc)
		pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
			if candidate != nil {
				t.sendICECandidate(candidate, "SUBSCRIBER")
			}
		})
		pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			utils.Debugf("[Telemost] subscriber PC state=%s [%s]", state, roleStr(t.isExitNode))
		})
	}

	if err := t.subscriberPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: sdpStr,
	}); err != nil {
		utils.Debugf("[Telemost] subscriber SetRemote: %v", err)
		return
	}
	answer, err := t.subscriberPC.CreateAnswer(nil)
	if err != nil {
		utils.Debugf("[Telemost] subscriber CreateAnswer: %v", err)
		return
	}
	// Форсируем recvonly для video, чтобы SFU создал subscriber video-track.
	answer.SDP = forceRecvonlyVideo(answer.SDP)
	if err := t.subscriberPC.SetLocalDescription(answer); err != nil {
		utils.Debugf("[Telemost] subscriber SetLocal: %v", err)
		return
	}
	mVideoA := strings.Count(answer.SDP, "m=video")
	mAudioA := strings.Count(answer.SDP, "m=audio")
	recvonly := strings.Count(answer.SDP, "a=recvonly")
	inactive := strings.Count(answer.SDP, "a=inactive")
	sendrecv := strings.Count(answer.SDP, "a=sendrecv")
	utils.Debugf("[Telemost] subscriber SDP answer: mVideo=%d mAudio=%d recvonly=%d sendrecv=%d inactive=%d",
		mVideoA, mAudioA, recvonly, sendrecv, inactive)
	utils.Debugf("[Telemost] subscriber answer media: %s [%s]",
		summarizeMediaDirections(answer.SDP), roleStr(t.isExitNode))
	if utils.IsVerbose() {
		utils.Verbosef("[Telemost] subscriber SDP answer:\n%s", answer.SDP)
	}
	t.sendSubscriberAnswer(answer.SDP, pcSeq)
}

func (t *TelemostTransport) sendSubscriberAnswer(sdpStr string, pcSeq int) error {
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"subscriberSdpAnswer": map[string]interface{}{
			"sdp": sdpStr, "pcSeq": pcSeq,
		},
	}
	data, _ := json.Marshal(msg)
	utils.Debugf("[Telemost] subscriberSdpAnswer -> pcSeq=%d", pcSeq)
	return t.writeWSMessage(websocket.TextMessage, data)
}

func (t *TelemostTransport) sendICECandidate(candidate *webrtc.ICECandidate, target string) error {
	init := candidate.ToJSON()
	seq := t.subSeq.Load()
	if target == "PUBLISHER" {
		seq = t.pubSeq.Load()
	}
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"webrtcIceCandidate": map[string]interface{}{
			"candidate":        init.Candidate,
			"sdpMid":           *init.SDPMid,
			"usernameFragment": *init.SDPMid,
			"sdpMlineIndex":    *init.SDPMLineIndex,
			"target":           target,
			"pcSeq":            seq,
		},
	}
	data, _ := json.Marshal(msg)
	return t.writeWSMessage(websocket.TextMessage, data)
}

func (t *TelemostTransport) handleICECandidate(m map[string]interface{}) {
	target, _ := m["target"].(string)
	pc := t.subscriberPC
	if target == "PUBLISHER" {
		pc = t.publisherPC
	}
	if pc == nil {
		return
	}
	cand, _ := m["candidate"].(string)
	mid, _ := m["sdpMid"].(string)
	var idx uint16
	if v, ok := m["sdpMlineIndex"].(float64); ok {
		idx = uint16(v)
	}
	init := webrtc.ICECandidateInit{Candidate: cand}
	if mid != "" {
		init.SDPMid = &mid
	}
	init.SDPMLineIndex = &idx
	if err := pc.AddICECandidate(init); err != nil {
		utils.Debugf("[Telemost] add ICE to %s: %v", target, err)
	}
}

// keyFrameGenerator is what both CanvasVideoGenerator (VP8) and
// VP9FrameGenerator (canvas_vp9.go) implement: wrap tunnel bytes in
// something that looks like a real keyframe of that codec.
type keyFrameGenerator interface {
	GenerateKeyFrame(data []byte) []byte
}

// keyFrameDecoder is the matching read side: CanvasVideoDecoder (VP8) and
// VP9FrameDecoder (canvas_vp9.go) both extract the bytes a keyFrameGenerator
// embedded.
type keyFrameDecoder interface {
	DecodeFrame(frameData []byte) ([]byte, error)
}

// frameEncoder holds the chosen generator, reused across sendFrame calls to
// avoid allocation overhead.
type frameEncoder struct {
	gen keyFrameGenerator
}

func (t *TelemostTransport) getEncoder() *frameEncoder {
	if t.encoder == nil {
		var gen keyFrameGenerator
		switch t.shareCodecMime {
		case webrtc.MimeTypeVP9:
			gen = &VP9FrameGenerator{}
		case webrtc.MimeTypeH264:
			gen = &H264FrameGenerator{}
		default:
			gen = &CanvasVideoGenerator{width: 1920, height: 1080}
		}
		t.encoder = &frameEncoder{gen: gen}
	}
	return t.encoder
}

func (e *frameEncoder) GenerateKeyFrame(data []byte) []byte {
	return e.gen.GenerateKeyFrame(data)
}

// ---- data path ----

func (t *TelemostTransport) Send(data []byte) error {
	if !t.IsConnected() {
		return fmt.Errorf("not connected")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case t.writeQueue <- cp:
		t.stats.PacketsSent.Add(1)
		t.stats.BytesSent.Add(uint64(len(data)))
		t.lastSendTime.Store(time.Now().UnixNano())
		return nil
	default:
		return fmt.Errorf("write queue full")
	}
}

func (t *TelemostTransport) Stats() transport.TransportStats {
	s := t.BaseTransport.Stats()
	s.BytesSent = t.stats.BytesSent.Load()
	s.BytesReceived = t.stats.BytesReceived.Load()
	s.PacketsSent = t.stats.PacketsSent.Load()
	s.PacketsRecv = t.stats.PacketsRecv.Load()
	s.Reconnects = t.stats.Reconnects.Load()
	s.Connected = t.IsConnected()
	return s
}

// sendFrame wraps one already-encoded wire frame in a VP8 keyframe and writes
// it to the track, once per layer declared in the offer: the SSRCs and RIDs
// come from videoTrackPlan, i.e. exactly the ones listed in a=ssrc: / a=rid:.
// TelemostTrack writes the RTP header verbatim, unlike TrackLocalStaticRTP
// which would overwrite the SSRC with its own.
//
// wire is NOT a tunnel packet - it is a complete wire-v2 batch frame, already
// built by the codec layer. BatchedTransport calls encodeBatch and then
// Transport.Send(encoded), so whatever lands here has been framed once already.
// Framing it a second time here produced 02 00 | 00 40 | <64-byte frame>, and
// the receiver's decodeBatch then read that inner frame as if it were a
// tunnel packet: the length prefix said 64 bytes, the bytes it consumed began
// with the real packet's own 02 00 00 3c IPv4 header, and the netstack was
// handed a packet whose version nibble was 0.
func (t *TelemostTransport) sendFrame(wire []byte) error {
	if len(wire) == 0 {
		return nil
	}
	if t.sharingTrack == nil || t.publishPlan == nil {
		return fmt.Errorf("publisher track not ready")
	}
	if !t.trackReady.Load() {
		return fmt.Errorf("publisher PC not connected")
	}

	enc := t.getEncoder()
	frame := enc.GenerateKeyFrame(wire)

	ts := t.nextShareTS()
	pt := t.sharePayloadT
	if pt == 0 {
		pt = uint8(t.sharingTrack.PayloadType())
	}
	if pt == 0 {
		pt = 96
	}

	var lastErr error
	sent := 0
	for _, layer := range t.publishPlan.Layers() {
		hdr := rtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: uint16(t.shareSeq.Add(1)),
			Timestamp:      ts,
			Marker:         true,
			SSRC:           layer.ssrc,
		}
		// RID: только там, где offer объявил a=rid:<x> send, и только под тем
		// extension id, который сам же и объявили в a=extmap. rtp.Extension has
		// unexported fields, so SetExtension is the only way in; it picks the
		// RFC 8285 one-byte profile for payloads of 16 bytes or less, which is
		// what every browser sends for a RID.
		if layer.rid != "" && t.shareRIDExtID > 0 {
			if err := hdr.SetExtension(uint8(t.shareRIDExtID), []byte(layer.rid)); err != nil {
				lastErr = fmt.Errorf("set RID ext %d=%q: %w", t.shareRIDExtID, layer.rid, err)
				continue
			}
			if hdr.ExtensionProfile != rfc8285OneByteHeader {
				lastErr = fmt.Errorf("RID ext profile is %#04x, want %#04x", hdr.ExtensionProfile, rfc8285OneByteHeader)
				continue
			}
		}

		pkt := rtp.Packet{Header: hdr, Payload: frame}
		if utils.Level() >= utils.LevelDebug {
			t.dumpRTP("->", &pkt)
		}
		n, err := t.sharingTrack.WriteHeader(hdr, frame)
		if err == nil && n > 0 && nackRTX {
			t.rtxRemember(hdr, frame)
		}
		if err != nil {
			t.stats.RTPSendFailed.Add(1)
			lastErr = fmt.Errorf("WriteRTP ssrc=%08x rid=%q: %w", layer.ssrc, layer.rid, err)
			continue
		}
		if n == 0 {
			// Pion's srtpWriterFuture drops writes silently before DTLS is up.
			lastErr = fmt.Errorf("WriteRTP ssrc=%08x dropped: DTLS/SRTP not ready", layer.ssrc)
			continue
		}
		sent++
	}
	if sent == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no layer written")
		}
		return lastErr
	}
	t.stats.RTPSendSuccess.Add(1)
	t.stats.VideoFramesSent.Add(1)
	return nil
}

// sendH264ParameterSets writes the fixed SPS and PPS NAL units
// (canvas_h264.go) once, before any IDR slice NAL. A real H264 stream is
// typically unparseable without them; since every frame this transport
// sends is independently a keyframe already (no inter-frame prediction to
// maintain), once is enough rather than repeating them with every slice the
// way some real encoders do for mid-stream random access.
func (t *TelemostTransport) sendH264ParameterSets() error {
	if t.sharingTrack == nil || t.publishPlan == nil {
		return fmt.Errorf("publisher track not ready")
	}
	pt := t.sharePayloadT
	if pt == 0 {
		pt = uint8(t.sharingTrack.PayloadType())
	}
	ts := t.shareTS.Load()
	var lastErr error
	sent := 0
	for _, nal := range [][]byte{H264SPS, H264PPS} {
		for _, layer := range t.publishPlan.Layers() {
			hdr := rtp.Header{
				Version:        2,
				PayloadType:    pt,
				SequenceNumber: uint16(t.shareSeq.Add(1)),
				Timestamp:      ts,
				SSRC:           layer.ssrc,
			}
			if _, err := t.sharingTrack.WriteHeader(hdr, nal); err != nil {
				lastErr = fmt.Errorf("WriteRTP ssrc=%08x: %w", layer.ssrc, err)
				continue
			}
			sent++
		}
	}
	if sent == 0 && lastErr != nil {
		return lastErr
	}
	utils.Debugf("[Telemost] H264 SPS/PPS sent (%d bytes/%d bytes) [%s]",
		len(H264SPS), len(H264PPS), roleStr(t.isExitNode))
	return nil
}

// dumpRTP печатает структуру RTP-пакета: заголовок + payload.
func (t *TelemostTransport) dumpRTP(dir string, pkt *rtp.Packet) {
	h := pkt.Header
	rid := ""
	if ids := h.GetExtensionIDs(); len(ids) > 0 {
		rid = string(h.GetExtension(ids[0]))
	}
	utils.Debugf("[RTP %s] seq=%d ts=%d ssrc=%08x pt=%d marker=%v ext=%d(%#04x) rid=%q len=%d [%s]",
		dir, h.SequenceNumber, h.Timestamp, h.SSRC, h.PayloadType,
		h.Marker, len(h.Extensions), h.ExtensionProfile, rid, len(pkt.Payload),
		roleStr(t.isExitNode))

	if utils.IsVerbose() {
		utils.Verbosef("[RTP %s] payload hex (%d bytes):\n%s",
			dir, len(pkt.Payload), hex.Dump(pkt.Payload))
	}
}

func (t *TelemostTransport) writerLoop() {
	for t.IsRunning() {
		select {
		case <-t.Done():
			return
		case pkt := <-t.writeQueue:
			t.enqueueFrame(pkt)
		}
	}
}

// maxFramesPerTick bounds how many buffered frames one tick of the publish
// clock may emit.
//
// Raised from 4 to 10 to 36 to 12, then dropped to 3 chasing a "small burst
// allowance" theory for why the reassembler kept losing the same fragment
// index - dropping it changed nothing (still lost, still that index), which
// ruled burst size out as the cause. The real cause fragment.go's padding
// now addresses directly, so this goes back to a byte-budget number: 12
// frames * 1392 usable bytes (maxVP8Payload minus the fragment header) *
// 120fps (publishFPS) ~= 2.0 MB/s (~16 Mbps), comfortable headroom over a
// ~10 Mbps target.
var maxFramesPerTick = envIntDefault("TELEMOST_FRAMES_PER_TICK", 12)

// maxQueuedFrames caps frameQueue (fragments) before the oldest are dropped.
var maxQueuedFrames = envIntDefault("TELEMOST_QUEUE", 4096)

// enqueueFrame hands a complete wire frame to the publish clock - the video
// one by default, or the audio one when shareCarrier=="audio" (see
// enqueueAudioFrames and runAudioPublisherKeepalive).
func (t *TelemostTransport) enqueueFrame(wire []byte) {
	if len(wire) == 0 {
		return
	}
	frags, err := FragmentFrame(wire)
	if err != nil {
		utils.Debugf("[Telemost] enqueueFrame: %v [%s]", err, roleStr(t.isExitNode))
		return
	}
	if t.shareCarrier == "audio" {
		t.enqueueAudioFrames(frags)
		return
	}
	t.frameMu.Lock()
	// The clock drains maxFramesPerTick per tick; a queue deeper than that
	// cannot be caught up on, so drop the oldest frame rather than accumulate
	// latency without bound. The tunnel above us sees the resulting gaps as
	// packet loss and retransmits, which is the honest outcome - silently
	// holding a growing backlog would instead turn into a stalled connection.
	maxQueued := maxQueuedFrames
	if len(t.frameQueue) >= maxQueued {
		dropped := len(t.frameQueue) - maxQueued + 1
		utils.Debugf("[Telemost] frameQueue full, dropping %d oldest fragment(s) [%s]",
			dropped, roleStr(t.isExitNode))
		copy(t.frameQueue, t.frameQueue[len(t.frameQueue)-maxQueued+1:])
		t.frameQueue = t.frameQueue[:maxQueued-1]
	}
	t.frameQueue = append(t.frameQueue, frags...)
	t.frameMu.Unlock()
}

// popFrames takes up to maxFramesPerTick buffered frames, oldest first.
func (t *TelemostTransport) popFrames() [][]byte {
	t.frameMu.Lock()
	defer t.frameMu.Unlock()
	n := len(t.frameQueue)
	if n > maxFramesPerTick {
		n = maxFramesPerTick
	}
	if n == 0 {
		return nil
	}
	out := make([][]byte, n)
	copy(out, t.frameQueue[:n])
	t.frameQueue = t.frameQueue[n:]
	return out
}

func (t *TelemostTransport) queuedFrames() int {
	t.frameMu.Lock()
	defer t.frameMu.Unlock()
	return len(t.frameQueue)
}

// maxAudioFramesPerTick bounds how many fragments one tick of
// runAudioPublisherKeepalive may emit. At audioFrameInterval (20ms, Opus's
// own native packetization rate) and maxFragmentBytes-sized fragments:
// 12 * 1392B * 50/s ~= 835 KB/s (~6.7 Mbps). Lower than the video carrier's
// budget only because the tick is slower (20ms vs ~8ms at publishFPS); this
// experiment is about whether the SFU treats audio differently at all, not
// about maximizing this path's ceiling yet.
const maxAudioFramesPerTick = 12

// enqueueAudioFrames is enqueueFrame's counterpart for the audio carrier.
func (t *TelemostTransport) enqueueAudioFrames(frags [][]byte) {
	t.audioFrameMu.Lock()
	defer t.audioFrameMu.Unlock()
	const maxQueued = 256
	if len(t.audioFrameQueue)+len(frags) > maxQueued {
		over := len(t.audioFrameQueue) + len(frags) - maxQueued
		if over > len(t.audioFrameQueue) {
			over = len(t.audioFrameQueue)
		}
		if over > 0 {
			utils.Debugf("[Telemost] audioFrameQueue full, dropping %d oldest fragment(s) [%s]",
				over, roleStr(t.isExitNode))
			copy(t.audioFrameQueue, t.audioFrameQueue[over:])
			t.audioFrameQueue = t.audioFrameQueue[:len(t.audioFrameQueue)-over]
		}
	}
	t.audioFrameQueue = append(t.audioFrameQueue, frags...)
}

// popAudioFrames takes up to maxAudioFramesPerTick buffered fragments,
// oldest first - popFrames' counterpart for the audio carrier.
func (t *TelemostTransport) popAudioFrames() [][]byte {
	t.audioFrameMu.Lock()
	defer t.audioFrameMu.Unlock()
	n := len(t.audioFrameQueue)
	if n > maxAudioFramesPerTick {
		n = maxAudioFramesPerTick
	}
	if n == 0 {
		return nil
	}
	out := make([][]byte, n)
	copy(out, t.audioFrameQueue[:n])
	t.audioFrameQueue = t.audioFrameQueue[n:]
	return out
}

// audioFillerPayload is the audio carrier's counterpart to keepalivePayload:
// a well-formed wire-v2 batch frame with zero records, so a tick with no
// real data to send still keeps the stream visibly alive rather than going
// quiet between bursts.
func audioFillerPayload(seq int) []byte {
	_ = seq
	return transport.EncodeBatchFrame(nil)
}

// runAudioPublisherKeepalive is runPublisherKeepalive's counterpart for the
// audio carrier: same shape (a fixed-rate clock, pacing a tick's fragments
// across it rather than bursting them, filler when the queue is empty), at
// Opus's own native 20ms packetization interval instead of a video frame
// interval.
func (t *TelemostTransport) runAudioPublisherKeepalive() {
	if !t.audioTrack.WaitBound(10 * time.Second) {
		return
	}
	const audioFrameInterval = 20 * time.Millisecond
	ticker := time.NewTicker(audioFrameInterval)
	defer ticker.Stop()
	sent := 0
	for range ticker.C {
		if !t.IsRunning() || !t.trackReady.Load() {
			continue
		}
		frames := t.popAudioFrames()
		if len(frames) == 0 {
			frames = [][]byte{audioFillerPayload(sent)}
		}
		gap := audioFrameInterval / time.Duration(len(frames))
		for i, payload := range frames {
			if i > 0 {
				time.Sleep(gap)
			}
			if err := t.sendAudioFrame(payload); err != nil {
				utils.Debugf("[Telemost] sendAudioFrame: %v", err)
			}
		}
		sent++
		if sent%150 == 0 {
			utils.Debugf("[Telemost] audio publisher keepalive: %d frames, %d queued [%s]",
				sent, len(t.audioFrameQueue), roleStr(t.isExitNode))
		}
	}
}

// sendAudioFrame wraps one already-fragmented wire chunk as a fake Opus
// packet (canvas_audio.go) and writes it to the audio track. Unlike
// sendFrame there are no simulcast layers to loop over - one SSRC, one
// packet.
func (t *TelemostTransport) sendAudioFrame(wire []byte) error {
	if len(wire) == 0 {
		return nil
	}
	if t.audioTrack == nil {
		return fmt.Errorf("audio track not ready")
	}
	if !t.trackReady.Load() {
		return fmt.Errorf("publisher PC not connected")
	}
	gen := &AudioFrameGenerator{}
	frame := gen.GenerateFrame(wire)

	// 20ms at the 48kHz clock Opus always uses, matching audioFrameInterval.
	ts := t.audioTS.Add(960)
	pt := uint8(t.audioTrack.PayloadType())
	hdr := rtp.Header{
		Version:        2,
		PayloadType:    pt,
		SequenceNumber: uint16(t.audioSeq.Add(1)),
		Timestamp:      ts,
		Marker:         true,
		SSRC:           uint32(t.audioTrack.SSRC()),
	}
	n, err := t.audioTrack.WriteHeader(hdr, frame)
	if err != nil {
		return fmt.Errorf("WriteRTP: %w", err)
	}
	if n == 0 {
		// Same srtpWriterFuture-before-DTLS-is-up case sendFrame guards
		// against for video.
		return fmt.Errorf("WriteRTP dropped: DTLS/SRTP not ready")
	}
	return nil
}

// sendREMBLoop periodically tells the SFU, via a Receiver Estimated Maximum
// Bitrate RTCP packet, that this subscriber's downlink can take `bps`. Some
// SFUs gate the forwarded bitrate on REMB rather than (or in addition to) TWCC;
// this opens that valve. Our datacenter downlink genuinely handles the value.
func (t *TelemostTransport) sendREMBLoop(mediaSSRC webrtc.SSRC, bps uint64) {
	pc := t.subscriberPC
	if pc == nil {
		return
	}
	utils.Debugf("[Telemost] REMB sender started: %d bps ssrc=%d [%s]",
		bps, uint32(mediaSSRC), roleStr(t.isExitNode))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for t.IsRunning() {
		if err := pc.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverEstimatedMaximumBitrate{
			Bitrate: float32(bps),
			SSRCs:   []uint32{uint32(mediaSSRC)},
		}}); err != nil {
			utils.Debugf("[Telemost] REMB WriteRTCP: %v [%s]", err, roleStr(t.isExitNode))
			return
		}
		<-ticker.C
	}
}

func (t *TelemostTransport) readSharingVideoTrack(track *webrtc.TrackRemote) {
	utils.Debugf("[Telemost] readSharingVideoTrack STARTED: id=%s codec=%s [%s]",
		track.ID(), track.Codec().MimeType, roleStr(t.isExitNode))
	defer func() {
		utils.Debugf("[Telemost] readSharingVideoTrack EXITED [%s]", roleStr(t.isExitNode))
	}()

	// Only VP8 and VP9 have a decoder here (CanvasVideoDecoder and
	// VP9FrameDecoder, canvas.go / canvas_vp9.go). For anything else we still
	// want to count received bytes for throughput stats, but we cannot
	// decode the frames, so we just accumulate raw payload sizes. Which one
	// to use is read from the track itself - Pion reports whatever the
	// publisher actually negotiated, so this needs no coordination with
	// shareCodecMime, which only governs what *this* side publishes.
	var decoder keyFrameDecoder
	switch track.Codec().MimeType {
	case webrtc.MimeTypeVP8:
		decoder = NewCanvasVideoDecoder(1920, 1080)
	case webrtc.MimeTypeVP9:
		decoder = NewVP9FrameDecoder()
	case webrtc.MimeTypeH264:
		decoder = NewH264FrameDecoder()
	}
	// Optionally drive the SFU's forward bitrate up with an explicit REMB in
	// case it gates on REMB rather than TWCC. Honest here: our downlink really
	// can take it. TELEMOST_SUB_REMB_KBPS=0 (default) disables.
	if v := envOrDefault("TELEMOST_SUB_REMB_KBPS", "100000"); v != "0" {
		if kbps, _ := strconv.Atoi(v); kbps > 0 {
			go t.sendREMBLoop(track.SSRC(), uint64(kbps)*1000)
		}
	}
	var reasm frameReassembler
	rtpBuf := make([]byte, 1500)
	for t.IsRunning() {
		n, _, err := track.Read(rtpBuf)
		if err != nil {
			utils.Debugf("[Telemost] readSharingVideoTrack: Read error: %v [%s]", err, roleStr(t.isExitNode))
			return
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(rtpBuf[:n]); err != nil {
			continue
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		// Always count received RTP payload bytes for throughput stats.
		t.stats.PacketsRecv.Add(1)
		t.stats.BytesReceived.Add(uint64(len(pkt.Payload)))
		t.lastRecvTime.Store(time.Now().UnixNano())

		if utils.Level() >= utils.LevelDebug {
			t.dumpRTP("<-", &pkt)
		}

		if decoder == nil {
			// Non-VP8 codec: we have no decoder, so just record the raw
			// payload as a "frame" for tunnel throughput purposes.
			t.RecordReceive(len(pkt.Payload))
			continue
		}

		body, err := decoder.DecodeFrame(pkt.Payload)
		if err != nil || len(body) == 0 {
			continue
		}
		data, ok := reasm.push(body, time.Now().UnixNano())
		if !ok {
			t.stats.VideoFramesRecv.Add(1)
			continue
		}
		t.RecordReceive(len(data))
		t.CallReceive(data)
	}
}

// readAudioCarrierTrack is readSharingVideoTrack's counterpart for the
// audio carrier (shareCarrier=="audio", see canvas_audio.go): the fragment
// reassembler is the exact same one video uses, since fragments carry their
// own identity (frameSeq, index, count - fragment.go) independent of
// whatever media they rode in on. Only the per-packet unwrap differs, and
// Opus needs far less of it than either video codec: no sync code to find,
// no bit-packed header to skip, just the one fixed TOC byte.
func (t *TelemostTransport) readAudioCarrierTrack(track *webrtc.TrackRemote) {
	utils.Debugf("[Telemost] readAudioCarrierTrack STARTED: id=%s codec=%s [%s]",
		track.ID(), track.Codec().MimeType, roleStr(t.isExitNode))
	defer func() {
		utils.Debugf("[Telemost] readAudioCarrierTrack EXITED [%s]", roleStr(t.isExitNode))
	}()

	decoder := NewAudioFrameDecoder()
	var reasm frameReassembler
	rtpBuf := make([]byte, 1500)
	for t.IsRunning() {
		n, _, err := track.Read(rtpBuf)
		if err != nil {
			utils.Debugf("[Telemost] readAudioCarrierTrack: Read error: %v [%s]", err, roleStr(t.isExitNode))
			return
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(rtpBuf[:n]); err != nil {
			continue
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		t.stats.PacketsRecv.Add(1)
		t.stats.BytesReceived.Add(uint64(len(pkt.Payload)))
		t.lastRecvTime.Store(time.Now().UnixNano())

		if utils.Level() >= utils.LevelDebug {
			t.dumpRTP("<-", &pkt)
		}

		body, err := decoder.DecodeFrame(pkt.Payload)
		if err != nil || len(body) == 0 {
			continue
		}
		data, ok := reasm.push(body, time.Now().UnixNano())
		if !ok {
			t.stats.VideoFramesRecv.Add(1)
			continue
		}
		t.RecordReceive(len(data))
		t.CallReceive(data)
	}
}

// defaultPingIntervalMs is what keepAliveLoop uses before serverHello has
// told it the real cadence (pingPongConfiguration.pingInterval).
const defaultPingIntervalMs = 5000

// keepAliveLoop sends the WS "ping" the signaling protocol expects.
//
// This used to tick at a flat 20s regardless of what the server asked for.
// serverHello advertises pingPongConfiguration - in practice
// {pingInterval:5000, ackTimeout:9000} - which this transport parsed into
// serverConfig but never consulted. Heartbeating four times slower than the
// server's own ackTimeout is what was reading, on both ends of the tunnel at
// once, as a dead session: every ~80s (four missed cycles at the old rate)
// the WS and both PeerConnections tore down and spent ~15-18s reconnecting -
// a full blackout for anything mid-handshake in the tunnel at that moment.
// Polling a short ticker and comparing elapsed time, rather than resizing the
// ticker itself, means a mid-session change to the server's advertised
// interval takes effect without recreating anything.
// telemetryLoop mirrors the web client's StatsReporter: every
// telemetryConfiguration.sendingInterval it sends a "telemetry" signaling
// message carrying getStats() of both PeerConnections as JSON strings.
// TELEMOST_TELEMETRY=0 disables it.
func (t *TelemostTransport) telemetryLoop(interval time.Duration) {
	if interval < 2*time.Second {
		interval = 10 * time.Second
	}
	collect := func(pc *webrtc.PeerConnection, sub bool) []string {
		out := []string{}
		if pc == nil {
			return out
		}
		for _, st := range pc.GetStats() {
			b, err := json.Marshal(st)
			if err != nil {
				continue
			}
			if sub {
				// Present received video as decoded frames, like a browser
				// that is actually rendering the stream.
				if in, ok := st.(webrtc.InboundRTPStreamStats); ok && in.Kind == "video" {
					m := map[string]interface{}{}
					_ = json.Unmarshal(b, &m)
					m["framesReceived"] = in.PacketsReceived
					m["framesDecoded"] = in.PacketsReceived
					m["keyFramesDecoded"] = in.PacketsReceived
					m["frameWidth"] = 1920
					m["frameHeight"] = 1080
					m["framesPerSecond"] = 30
					b, _ = json.Marshal(m)
				}
			}
			out = append(out, string(b))
		}
		return out
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-t.Done():
			return
		case <-tick.C:
		}
		body := map[string]interface{}{
			"publisherRawStatsReport":  collect(t.publisherPC, false),
			"subscriberRawStatsReport": collect(t.subscriberPC, true),
			"roomAgentRawStatsReport":  []string{},
			"eventsReport":             []interface{}{},
			"customStats": map[string]interface{}{
				"clientCpuStats":              []interface{}{},
				"publisherAudioProcessing":    []interface{}{},
				"publisherVideoEncoding":      []interface{}{},
				"publisherVideoProcessing":    []interface{}{},
				"subscriberVideoProcessing":   []interface{}{},
				"applyDevicesStats":           []interface{}{},
				"slotSubscriberVideoDecoding": []interface{}{},
				"setSlotsResponse":            []interface{}{},
			},
		}
		data, _ := json.Marshal(map[string]interface{}{"uid": uuid.New().String(), "telemetry": body})
		if err := t.writeWSMessage(websocket.TextMessage, data); err != nil {
			utils.Debugf("[Telemost] telemetry: %v [%s]", err, roleStr(t.isExitNode))
		} else {
			utils.Debugf("[Telemost] telemetry -> %d bytes [%s]", len(data), roleStr(t.isExitNode))
		}
	}
}

func (t *TelemostTransport) keepAliveLoop() {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-t.Done():
			return
		case <-tick.C:
			interval := time.Duration(t.pingIntervalMs.Load()) * time.Millisecond
			if interval <= 0 {
				interval = defaultPingIntervalMs * time.Millisecond
			}
			// Clamped so a missing or malformed server value can't make this
			// busier than once a second or, at the other end, any quieter
			// than the old fixed interval.
			if interval < time.Second {
				interval = time.Second
			} else if interval > 20*time.Second {
				interval = 20 * time.Second
			}
			if time.Since(last) < interval {
				continue
			}
			last = time.Now()
			msg := map[string]interface{}{"uid": uuid.New().String(), "ping": map[string]interface{}{}}
			data, _ := json.Marshal(msg)
			_ = t.writeWSMessage(websocket.TextMessage, data)
		}
	}
}

func extractUserIDFromCookie(cookies string) string {
	for _, part := range strings.Split(cookies, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "Session_id=") {
			return strings.TrimPrefix(part, "Session_id=")
		}
	}
	return ""
}

var _ = base64.StdEncoding

// forceRecvonlyVideo выставляет a=recvonly для всех m=video в SDP.
// Без этого SFU не создаёт subscriber video-track, и данные не приходят.
func forceRecvonlyVideo(sdpStr string) string {
	lines := strings.Split(sdpStr, "\r\n")
	var out []string
	inVideo := false
	for _, l := range lines {
		if strings.HasPrefix(l, "m=video") {
			inVideo = true
			out = append(out, l)
			continue
		}
		if strings.HasPrefix(l, "m=") {
			inVideo = false
			out = append(out, l)
			continue
		}
		if inVideo && (l == "a=sendrecv" || l == "a=sendonly" || l == "a=inactive") {
			out = append(out, "a=recvonly")
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\r\n")
}

// observeInboundDataChannel logs any DataChannel the SFU opens towards us.
//
// The DataChannel is the only path in this transport that does not pass through
// the SFU's video pipeline, so whether the SFU forwards one decides whether the
// tunnel can ever outgrow the frame rate. It was never wired up on the
// subscriber side, so a forwarded channel looked identical to no channel at all.
func (t *TelemostTransport) observeInboundDataChannel(pc *webrtc.PeerConnection) {
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		label := dc.Label()
		utils.Debugf("[Telemost] inbound DataChannel: label=%q [%s]", label, roleStr(t.isExitNode))
		dc.OnOpen(func() {
			utils.Debugf("[Telemost] inbound DataChannel open: label=%q [%s]", label, roleStr(t.isExitNode))
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			utils.Debugf("[Telemost] inbound DC <- %d bytes: %q [%s]",
				len(msg.Data), truncHex(msg.Data, 24), roleStr(t.isExitNode))
		})
		dc.OnClose(func() {
			utils.Debugf("[Telemost] inbound DataChannel closed: label=%q [%s]", label, roleStr(t.isExitNode))
		})
		dc.OnError(func(err error) {
			utils.Debugf("[Telemost] inbound DataChannel error: %v [%s]", err, roleStr(t.isExitNode))
		})
	})
}

// runDataChannelProbe writes a counted marker to the sharing DataChannel once a
// second, so a forwarded message can be told apart from a dropped one.
//
// The room advertises "dataChannelSharing":"TO_RTP", which says the SFU expects
// to carry a sharing payload over this channel. But the only evidence available
// before this probe was that the channel opened, and opening proves nothing
// about whether the other participant ever receives what we write.
func (t *TelemostTransport) runDataChannelProbe() {
	dc := t.sharingDC
	if dc == nil {
		utils.Debugf("[Telemost] DC probe: no sharing DataChannel [%s]", roleStr(t.isExitNode))
		return
	}
	for seq := 0; ; seq++ {
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		payload := []byte(fmt.Sprintf("dc-probe-%s-%d", roleStr(t.isExitNode), seq))
		if err := dc.Send(payload); err != nil {
			utils.Debugf("[Telemost] DC probe send failed: %v [%s]", err, roleStr(t.isExitNode))
		} else {
			utils.Debugf("[Telemost] DC probe -> %d bytes: %q [%s]",
				len(payload), truncHex(payload, 24), roleStr(t.isExitNode))
		}
		time.Sleep(time.Second)
	}
}
