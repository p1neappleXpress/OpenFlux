package ok

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// OK web push (NewsFetchCoordinator.js):
//
//	POST /push?cmd=PeriodicManager&gwt.requested=<gwtHash>[&sse=true]&p_sId=<id>
//	Content-Type: application/x-www-form-urlencoded
//	body: <extra params>&blocks=MainFeedsNewFeedPush,PS,VCP,...&p_NLP=<ts>
//
// Response is a long-lived stream (SSE mode or long-poll). Chunks are
// delimited by <!--SSP--> (and joined with <!--SP--> in multi-tab mode).
// Header Rendered-Blocks lists which subscribed blocks were rendered.
//
// VCP = Video Call Push — the block that carries incoming-call notifications
// which the calls bootstrap turns into onPush → VideoChatCall INCOMING.
// Exact VCP HTML/JSON payload shape is still best confirmed from a live
// capture with adblock disabled; parsing below is best-effort.

const (
	pushPath          = "https://ok.ru/push"
	pushChunkDelim    = "<!--SSP-->"
	pushJoinDelim     = "<!--SP-->"
	defaultPushBlocks = "MainFeedsNewFeedPush,PS,VCP,DPC,NTF,FeedbackGrowl,MP,FSC,MPC,MPC"
)

var (
	reUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reUID  = regexp.MustCompile(`(?i)(?:opponent(?:Id)?|userId|fromUserId|callerId)["\s:=]+(\d{6,})`)
)

// PushConfig holds session bits needed to open PeriodicManager.
type PushConfig struct {
	GWTRequested string // window.pageCtx.gwtHash / gwt.requested
	SessionID    string // p_sId
	Blocks       string // comma-separated; must include VCP for calls
	NLP          string // p_NLP watermark (optional)
	UseSSE       bool   // &sse=true (preferred; matches browser)
}

// WaitIncomingCall opens OK PeriodicManager push and waits for a VCP-like
// chunk that yields a conversationId (and optionally opponent uid).
//
// Requires cookies on OkClient and a valid PushConfig (at least GWTRequested).
// This replaces the previous hard stub for exit-side ring detection.
func (c *OkClient) WaitIncomingCall() (CallParams, error) {
	cfg := c.pushConfig
	if cfg == nil {
		return CallParams{}, fmt.Errorf("ok: push config not set (need gwt.requested / p_sId from browser)")
	}
	return c.WaitIncomingCallPush(*cfg, 0)
}

// SetPushConfig stores PeriodicManager parameters for the exit waiter.
func (c *OkClient) SetPushConfig(cfg PushConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := cfg
	if cp.Blocks == "" {
		cp.Blocks = defaultPushBlocks
	}
	if !strings.Contains(cp.Blocks, "VCP") {
		cp.Blocks = strings.Trim(cp.Blocks, ",") + ",VCP"
	}
	c.pushConfig = &cp
}

// WaitIncomingCallPush runs until a plausible incoming-call payload is seen
// or timeout elapses (0 = no timeout).
func (c *OkClient) WaitIncomingCallPush(cfg PushConfig, timeout time.Duration) (CallParams, error) {
	if cfg.GWTRequested == "" {
		return CallParams{}, fmt.Errorf("ok: PushConfig.GWTRequested required")
	}
	if cfg.Blocks == "" {
		cfg.Blocks = defaultPushBlocks
	}
	if !cfg.UseSSE {
		cfg.UseSSE = true
	}

	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return CallParams{}, fmt.Errorf("ok: push wait timeout")
		}
		params, err := c.onePushSession(cfg, deadline)
		if err == nil && params.ConvID != "" {
			return params, nil
		}
		if err != nil {
			logInfo("push session ended: %v — reconnect", err)
		}
		time.Sleep(1 * time.Second)
	}
}

func (c *OkClient) onePushSession(cfg PushConfig, deadline time.Time) (CallParams, error) {
	q := url.Values{}
	q.Set("cmd", "PeriodicManager")
	q.Set("gwt.requested", cfg.GWTRequested)
	if cfg.UseSSE {
		q.Set("sse", "true")
	}
	if cfg.SessionID != "" {
		q.Set("p_sId", cfg.SessionID)
	}
	rawURL := pushPath + "?" + q.Encode()

	form := url.Values{}
	form.Set("blocks", cfg.Blocks)
	if cfg.NLP != "" {
		form.Set("p_NLP", cfg.NLP)
	}
	// Browser also sends timing fields (st.mpCheckTime, …); optional for ring.

	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return CallParams{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Origin", okOrigin)
	req.Header.Set("Referer", okOrigin+"/")
	if c.cookies != "" {
		req.Header.Set("Cookie", c.cookies)
	}

	client := &http.Client{
		// No overall Timeout — this is a long-lived stream (like browser SSE).
		Timeout: 0,
	}
	if !deadline.IsZero() {
		// Bound the whole session when caller set a deadline.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return CallParams{}, fmt.Errorf("ok: push deadline passed")
		}
		client.Timeout = remaining
	}

	logInfo("push open %s blocks=%s", rawURL, cfg.Blocks)
	resp, err := client.Do(req)
	if err != nil {
		return CallParams{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return CallParams{}, fmt.Errorf("push HTTP %d: %s", resp.StatusCode, string(b))
	}

	rendered := resp.Header.Get("Rendered-Blocks")
	if rendered != "" {
		logInfo("push Rendered-Blocks: %s", rendered)
	}

	return readPushStream(resp.Body, cfg)
}

// readPushStream splits on <!--SSP--> (NewsFetchCoordinator) and scans chunks.
func readPushStream(r io.Reader, cfg PushConfig) (CallParams, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, err := br.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			for {
				s := buf.String()
				idx := strings.Index(s, pushChunkDelim)
				if idx < 0 {
					break
				}
				chunk := s[:idx]
				buf.Reset()
				buf.WriteString(s[idx+len(pushChunkDelim):])
				// Multi-tab master prefix: id<!--SP-->payload
				if i := strings.Index(chunk, pushJoinDelim); i >= 0 {
					chunk = chunk[i+len(pushJoinDelim):]
				}
				if p, ok := parseVCPChunk(chunk); ok {
					logInfo("push VCP-like chunk conv=%s opponent=%s", p.ConvID, p.UserID)
					return p, nil
				}
			}
		}
		if err == io.EOF {
			return CallParams{}, fmt.Errorf("push stream EOF")
		}
		if err != nil {
			return CallParams{}, err
		}
	}
}

// parseVCPChunk best-effort extraction from a PeriodicManager HTML/JSON chunk.
// Live VCP markup varies; we accept any chunk that contains a conversation UUID
// and looks call-related (VCP / videochat / conversation / call).
func parseVCPChunk(chunk string) (CallParams, bool) {
	low := strings.ToLower(chunk)
	callish := strings.Contains(low, "vcp") ||
		strings.Contains(low, "videochat") ||
		strings.Contains(low, "conversation") ||
		strings.Contains(low, "videocall") ||
		strings.Contains(chunk, "VideoChat")
	if !callish {
		return CallParams{}, false
	}
	m := reUUID.FindString(chunk)
	if m == "" {
		return CallParams{}, false
	}
	out := CallParams{ConvID: m}
	if u := reUID.FindStringSubmatch(chunk); len(u) > 1 {
		out.UserID = u[1]
	}
	return out, true
}

// DefaultExitPushBlocks is the browser block list including VCP.
func DefaultExitPushBlocks() string { return defaultPushBlocks }
