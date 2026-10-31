package ok

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// OK Calls API (https://calls.okcdn.ru/fb.do) — captured from browser.
//
// application_key for OK portal calls (not MAX CNHIJPLGDIHBABABA):
const callsApplicationKey = "CPCCMQKGDIHBABABA"

const callsFBDO = "https://calls.okcdn.ru/fb.do"

// CallsSession is returned by auth.anonymLogin on calls.okcdn.ru.
type CallsSession struct {
	UID              string `json:"uid"`
	SessionKey       string `json:"session_key"`
	SessionSecretKey string `json:"session_secret_key"`
	APIServer        string `json:"api_server"`
	ExternalUserID   string `json:"external_user_id"`
}

// MediaParams is the response that yields videowebrtc + TURN (after login).
// Capture shape:
//
//	{
//	  "token": "...",
//	  "endpoint": "wss://videowebrtc.okcdn.ru/ws2",
//	  "turn_server": { "urls": [...], "username": "ts:uid", "credential": "..." },
//	  "stun_server": { "urls": [...] },
//	  "client_type": "PORTAL",
//	  "external_user_type": "ODKL"
//	}
type MediaParams struct {
	Token            string `json:"token"`
	Endpoint         string `json:"endpoint"`
	WTEndpoint       string `json:"wt_endpoint"`
	ClientType       string `json:"client_type"`
	DeviceIdx        int    `json:"device_idx"`
	ExternalUserType string `json:"external_user_type"`
	ServerTime       int64  `json:"server_time"`
	TurnServer       struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username"`
		Credential string   `json:"credential"`
	} `json:"turn_server"`
	StunServer struct {
		URLs []string `json:"urls"`
	} `json:"stun_server"`
}

// AuthAnonymLogin posts method=auth.anonymLogin to calls.okcdn.ru/fb.do.
//
// session_data is URL-encoded JSON, e.g.:
//
//	{"version":3,"device_id":"...","client_version":1.1,"client_type":"SDK_JS","auth_token":"$..."}
//
// auth_token here often comes from getCallToken result.token ($...) or from
// an earlier OK/MAX-style anonym token — confirm which one the browser sends.
func (c *OkClient) AuthAnonymLogin(authToken, deviceID string) (CallsSession, error) {
	var sess CallsSession
	if deviceID == "" {
		deviceID = c.deviceID
	}
	sessionData, _ := json.Marshal(map[string]interface{}{
		"version":        3,
		"device_id":      deviceID,
		"client_version": 1.1,
		"client_type":    "SDK_JS",
		"auth_token":     authToken,
	})

	form := url.Values{}
	form.Set("session_data", string(sessionData))
	form.Set("method", "auth.anonymLogin")
	form.Set("format", "JSON")
	form.Set("application_key", callsApplicationKey)

	body, code, err := c.doForm(http.MethodPost, callsFBDO, form)
	if err != nil {
		return sess, err
	}
	if code != 200 {
		return sess, fmt.Errorf("auth.anonymLogin HTTP %d: %s", code, truncate(body, 300))
	}
	if err := json.Unmarshal(body, &sess); err != nil {
		return sess, fmt.Errorf("auth.anonymLogin JSON: %w body=%s", err, truncate(body, 300))
	}
	if sess.SessionKey == "" && sess.UID == "" {
		return sess, fmt.Errorf("auth.anonymLogin empty session: %s", truncate(body, 400))
	}
	logInfo("calls auth.anonymLogin uid=%s api=%s", sess.UID, sess.APIServer)
	c.mu.Lock()
	c.callsSession = &sess
	c.mu.Unlock()
	return sess, nil
}

// SystemGetInfo — method=system.getInfo (keepalive / clock).
func (c *OkClient) SystemGetInfo(sessionKey string) error {
	form := url.Values{}
	form.Set("method", "system.getInfo")
	form.Set("format", "JSON")
	form.Set("application_key", callsApplicationKey)
	form.Set("session_key", sessionKey)
	body, code, err := c.doForm(http.MethodPost, callsFBDO, form)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("system.getInfo HTTP %d: %s", code, truncate(body, 200))
	}
	logInfo("system.getInfo: %s", truncate(body, 120))
	return nil
}

// VchatGetConversationParams — method=vchat.getConversationParams
// conversationId + session_key → token, endpoint, TURN, client_type PORTAL.
func (c *OkClient) VchatGetConversationParams(sessionKey, conversationID string) (CallParams, error) {
	form := url.Values{}
	form.Set("method", "vchat.getConversationParams")
	form.Set("format", "JSON")
	form.Set("application_key", callsApplicationKey)
	form.Set("session_key", sessionKey)
	form.Set("conversationId", conversationID)
	body, code, err := c.doForm(http.MethodPost, callsFBDO, form)
	if err != nil {
		return CallParams{}, err
	}
	if code != 200 {
		return CallParams{}, fmt.Errorf("vchat.getConversationParams HTTP %d: %s", code, truncate(body, 300))
	}
	_, out, err := ParseMediaParams(body)
	if err != nil {
		return out, err
	}
	out.ConvID = conversationID
	return out, nil
}

// ParseMediaParams unmarshals the videowebrtc/TURN payload from calls API.
func ParseMediaParams(raw []byte) (MediaParams, CallParams, error) {
	var m MediaParams
	var out CallParams
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, out, err
	}
	if m.Token == "" || m.Endpoint == "" {
		return m, out, fmt.Errorf("media params missing token/endpoint: %s", truncate(raw, 300))
	}
	out.Token = m.Token
	out.Endpoint = strings.TrimRight(m.Endpoint, "/")
	out.TurnURLs = m.TurnServer.URLs
	out.TurnUser = m.TurnServer.Username
	out.TurnPass = m.TurnServer.Credential
	out.StunURLs = m.StunServer.URLs
	// turn username often "timestamp:userId"
	if i := strings.LastIndex(m.TurnServer.Username, ":"); i >= 0 {
		out.UserID = m.TurnServer.Username[i+1:]
	}
	logInfo("media params endpoint=%s client=%s turn=%v", out.Endpoint, m.ClientType, out.TurnURLs)
	return m, out, nil
}

// doForm posts application/x-www-form-urlencoded to calls.okcdn.ru.
func (c *OkClient) doForm(method, rawURL string, form url.Values) ([]byte, int, error) {
	return c.doJSON(method, rawURL, []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       "https://ok.ru",
		"Referer":      "https://ok.ru/",
	})
}

// BuildWS2URL joins media endpoint + token + user + conversation into PORTAL ws2 URL.
func BuildWS2URL(p CallParams, tgt string) string {
	return craftPortalEndpoint(p, tgt)
}
