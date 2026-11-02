package mtslink

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/pion/webrtc/v3"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// getEventSessionID resolves the actual eventSessionId from API
func (t *MTSLinkWebRTCTransport) getEventSessionID() (string, error) {
	// URL formats:
	// https://my.mts-link.ru/j/{eventID}/{eventSessionID}/session/{sessionID}
	// https://my.mts-link.ru/j/{eventID}/{shortEventId}

	re1 := regexp.MustCompile(`/j/(\d+)/(\d+)/session/(\d+)`)
	matches := re1.FindStringSubmatch(t.meetingURL)
	if len(matches) == 4 {
		// Full format - eventSessionID is in the URL
		return matches[2], nil
	}

	// Short format - need to fetch from API
	re2 := regexp.MustCompile(`/j/(\d+)/(\d+)`)
	matches = re2.FindStringSubmatch(t.meetingURL)
	if len(matches) != 3 {
		return "", fmt.Errorf("invalid meeting URL format")
	}

	eventID := matches[2]

	// Fetch event info from API
	apiURL := fmt.Sprintf("https://gw.mts-link.ru/api/event/%s", eventID)
	req, _ := http.NewRequest("GET", apiURL, nil)
	t.addCommonHeaders(req)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch event info: %w", err)
	}
	defer resp.Body.Close()

	body, err := readResponseBody(resp)
	if err != nil {
		return "", fmt.Errorf("read event response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fetch event info failed, status %d", resp.StatusCode)
	}

	var eventResp EventResponse
	if err := json.Unmarshal(body, &eventResp); err != nil {
		return "", fmt.Errorf("parse event response: %w", err)
	}

	if len(eventResp.EventSessions) == 0 {
		return "", fmt.Errorf("no event sessions found")
	}

	// Return the first event session ID
	eventSessionID := fmt.Sprintf("%d", eventResp.EventSessions[0].ID)
	return eventSessionID, nil
}

//loadInitialPage loads the meeting page
func (t *MTSLinkWebRTCTransport) loadInitialPage() error {
	req, _ := http.NewRequest("GET", t.meetingURL, nil)
	req.Header.Set("User-Agent", webrtcUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("page load failed: status %d", resp.StatusCode)
	}

	// Discard body, we just need cookies
	io.Copy(io.Discard, resp.Body)
	return nil
}

// guestLogin performs guest authentication
func (t *MTSLinkWebRTCTransport) guestLogin(eventSessionID string) error {
	apiURL := fmt.Sprintf("https://gw.mts-link.ru/api/eventsessions/%s/guestlogin", eventSessionID)

	formData := url.Values{}
	formData.Set("nickname", t.userName)
	formData.Set("name", "")
	formData.Set("secondName", "")
	formData.Set("phone", "")

	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(formData.Encode()))
	t.addCommonHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Origin", "https://my.mts-link.ru")
	req.Header.Set("Referer", "https://my.mts-link.ru/")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("guest login: %w", err)
	}
	defer resp.Body.Close()

	body, err := readResponseBody(resp)
	if err != nil {
		return fmt.Errorf("read guest login response: %w", err)
	}

	if resp.StatusCode == 404 {
		return fmt.Errorf("meeting not found (404) - meeting may have ended")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("guest login failed: status %d: %s", resp.StatusCode, string(body))
	}

	var loginResp GuestLoginResponse
	if err := json.Unmarshal(body, &loginResp); err != nil {
		return fmt.Errorf("parse guest login response: %w", err)
	}

	t.userId = fmt.Sprintf("%d", loginResp.Participation.User.ID)
	t.sessionID = loginResp.User.SessionID
	return nil
}

// confirmLogin confirms the session
func (t *MTSLinkWebRTCTransport) confirmLogin() error {
	req, _ := http.NewRequest("GET", "https://gw.mts-link.ru/api/login", nil)
	t.addCommonHeaders(req)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("confirm login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("confirm login failed: status %d", resp.StatusCode)
	}

	io.Copy(io.Discard, resp.Body)
	return nil
}

// createConference creates a conference
func (t *MTSLinkWebRTCTransport) createConference(eventSessionID string) error {
	apiURL := fmt.Sprintf("https://gw.mts-link.ru/api/eventsessions/%s/conferences", eventSessionID)

	formData := url.Values{}
	formData.Set("options", `{"audioVideo":true,"audioBandwidthSettings":{"video":{"enabled":false}}}`)

	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(formData.Encode()))
	t.addCommonHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://my.mts-link.ru")
	req.Header.Set("Referer", "https://my.mts-link.ru/")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("create conference: %w", err)
	}
	defer resp.Body.Close()

	body, err := readResponseBody(resp)
	if err != nil {
		return fmt.Errorf("read conference response: %w", err)
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("create conference failed: status %d: %s", resp.StatusCode, string(body))
	}

	var confResp ConferenceResponse
	if err := json.Unmarshal(body, &confResp); err != nil {
		return fmt.Errorf("parse conference response: %w", err)
	}

	t.conferenceID = fmt.Sprintf("%d", confResp.ID)
	t.publishToken = confResp.PrivateKey
	return nil
}

// registerConnection registers the connection session (required before WebSocket)
func (t *MTSLinkWebRTCTransport) registerConnection(eventSessionID string) error {
	apiURL := fmt.Sprintf("https://gw.mts-link.ru/api/eventsessions/%s/connections", eventSessionID)

	req, _ := http.NewRequest("POST", apiURL, nil)
	t.addCommonHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("X-User-Id", t.userId)
	req.Header.Set("Origin", "https://my.mts-link.ru")
	req.Header.Set("Referer", "https://my.mts-link.ru/")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("register connection: %w", err)
	}
	defer resp.Body.Close()

	body, err := readResponseBody(resp)
	if err != nil {
		return fmt.Errorf("read connection response: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("register connection failed: status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response to get participation ID
	var connResp struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(body, &connResp); err != nil {
		return fmt.Errorf("parse connection response: %w", err)
	}

	t.participationID = fmt.Sprintf("%d", connResp.ID)
	utils.Debugf("[MTS-WebRTC] Participation ID: %s", t.participationID)

	return nil
}

// getJoinToken obtains the WebRTC join token
func (t *MTSLinkWebRTCTransport) getJoinToken(eventSessionID string) error {
	apiURL := fmt.Sprintf("https://gw.mts-link.ru/api/eventsessions/%s/join-token", eventSessionID)

	req, _ := http.NewRequest("GET", apiURL, nil)
	t.addCommonHeaders(req)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("get join token: %w", err)
	}
	defer resp.Body.Close()

	body, err := readResponseBody(resp)
	if err != nil {
		return fmt.Errorf("read join token response: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("get join token failed: status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp JoinTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parse join token response: %w", err)
	}

	t.joinToken = tokenResp.Data.JoinToken
	if t.joinToken == "" {
		return fmt.Errorf("empty join token in response")
	}
	return nil
}

// joinSFU establishes WebRTC connection with DataChannel
func (t *MTSLinkWebRTCTransport) joinSFU(eventSessionID string) error {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	t.peerConnection = pc

	// Log connection state changes
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		utils.Debugf("[MTS-WebRTC] PeerConnection state: %s", state.String())
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		utils.Debugf("[MTS-WebRTC] ICE connection state: %s", state.String())
	})

	pc.OnICEGatheringStateChange(func(state webrtc.ICEGathererState) {
		utils.Debugf("[MTS-WebRTC] ICE gathering state: %s", state.String())
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			utils.Debugf("[MTS-WebRTC] ICE candidate: %s", candidate.String())
		}
	})

	// Create DataChannel for tunnel data
	dc, err := pc.CreateDataChannel("tunnel", nil)
	if err != nil {
		return fmt.Errorf("create datachannel: %w", err)
	}
	t.dataChannel = dc

	// Setup DataChannel handlers
	dc.OnOpen(func() {
		utils.Debugf("[MTS-WebRTC] DataChannel opened")
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		utils.Debugf("[MTS-WebRTC] <- DataChannel message: len=%d isString=%v", len(msg.Data), msg.IsString)
		if msg.IsString {
			// String data - probably signaling
			utils.Debugf("[MTS-WebRTC] String message: %s", string(msg.Data))
		} else {
			// Binary data - tunnel packet
			t.RecordReceive(len(msg.Data))
			t.onDataMu.RLock()
			cb := t.onData
			t.onDataMu.RUnlock()
			if cb != nil {
				cb(msg.Data)
			}
		}
	})

	dc.OnClose(func() {
		utils.Debugf("[MTS-WebRTC] DataChannel closed")
	})

	dc.OnError(func(err error) {
		utils.Debugf("[MTS-WebRTC] DataChannel error: %v", err)
	})

	// Add single audio transceiver (recvonly to satisfy MTS-Link)
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return fmt.Errorf("add audio transceiver: %w", err)
	}

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	// Send offer to SFU
	sfuURL := fmt.Sprintf("https://sfu.mts-link.ru/rtc/room/%s/join", eventSessionID)
	params := url.Values{}
	params.Add("userId", t.userId)
	params.Add("joinToken", t.joinToken)
	params.Add("disableRecording", "false")
	params.Add("userName", t.userName)
	if t.publishToken != "" {
		params.Add("publishToken", t.publishToken)
	}
	params.Add("twcc", "true")
	params.Add("originMid", "0")
	params.Add("preferredPublishCodec", "vp9")
	params.Add("did", t.deviceID)
	params.Add("pid", t.deviceID+"8")

	sfuURL += "?" + params.Encode()

	req, _ := http.NewRequest("POST", sfuURL, strings.NewReader(offer.SDP))
	t.addCommonHeaders(req)
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send offer to SFU: %w", err)
	}
	defer resp.Body.Close()

	sdpBody, err := readResponseBody(resp)
	if err != nil {
		return fmt.Errorf("read SFU response: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("SFU join failed: status %d: %s", resp.StatusCode, string(sdpBody))
	}

	// Set remote description
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  string(sdpBody),
	}

	if err := pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	return nil
}

// writerLoop sends queued packets through DataChannel
func (t *MTSLinkWebRTCTransport) writerLoop() {
	for {
		select {
		case <-t.done:
			return
		case pkt := <-t.sendQueue:
			if t.dataChannel == nil || t.dataChannel.ReadyState() != webrtc.DataChannelStateOpen {
				utils.Debugf("[MTS-WebRTC] -> DataChannel not ready (state=%v), dropping packet len=%d",
					t.dataChannel.ReadyState(), len(pkt))
				continue
			}

			utils.Debugf("[MTS-WebRTC] -> Sending packet len=%d", len(pkt))
			if err := t.dataChannel.Send(pkt); err != nil {
				utils.Debugf("[MTS-WebRTC] send error: %v", err)
				continue
			}

			t.RecordSend(len(pkt))
			utils.Debugf("[MTS-WebRTC] -> Sent successfully")
		}
	}
}
