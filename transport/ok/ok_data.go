package ok

import (
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// CallParams across the OK call pipeline.
// Step1 getCallToken → APIURL (calls.okcdn.ru) + Token ($...).
// Step2 (TODO) start/join on APIURL → ConvID + videowebrtc ws2 + TURN.
type CallParams struct {
	APIURL   string // https://calls.okcdn.ru
	Endpoint string // later: wss://videowebrtc... or full ws2
	Token    string
	UserID   string
	ConvID   string
	TurnURLs []string
	TurnUser string
	TurnPass string
	StunURLs []string
}

// OkClient holds ok.ru browser cookies + optional calls.okcdn.ru session + push.
type OkClient struct {
	cookies      string
	ua           string
	deviceID      string
	mu           sync.Mutex
	callsSession *CallsSession // set after auth.anonymLogin on calls.okcdn.ru
	pushConfig   *PushConfig   // PeriodicManager SSE (NewsFetchCoordinator)
}

// CallHandler mirrors oneme.CallHandler — media path over videowebrtc.
type CallHandler struct {
	tag               string
	role              string // "caller" | "receiver"
	pc                *webrtc.PeerConnection
	dc                *webrtc.DataChannel
	conn              *websocket.Conn
	localID           int64
	remoteID          int64
	seq               int64
	onConnected       func()
	callAccepted      bool
	acceptSent        bool
	hasRemoteDesc     bool
	pendingCandidates []map[string]interface{}
	dcInbound         func([]byte)
	msgHandler        func(string)
	reconnectCh       chan struct{}
	mu                sync.Mutex
}

// useICEInjection: same trick as oneme — tunnel bytes inside ICE candidate base64
// instead of (or in addition to) DataChannel. Keep true for parity with oneme.
var useICEInjection = true

// sequence counter helper for signaling commands
func nextSeq(h *CallHandler) int64 {
	s := h.seq
	h.seq++
	return s
}

// atomic flag if needed later
var _ = atomic.Int64{}
