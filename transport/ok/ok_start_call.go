package ok

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// StartOutgoingVideoChatCall places an OK portal call via the classic /dk command.
//
// Capture:
//
//	https://ok.ru/dk
//	  cmd=VideoChatCall
//	  st.call.ft=CALL_AUDIO
//	  st.call.dir=OUTGOING
//	  st.call.opponentId=<peer>
//	  gwt.requested=<...>
//
// Response is HTML (call2 panel). conversationId / vcid is obtained from
// subsequent calls.okcdn.ru traffic (or parsed from page if present).
func (c *OkClient) StartOutgoingVideoChatCall(opponentID int64, gwtRequested string) (string, error) {
	form := url.Values{}
	form.Set("cmd", "VideoChatCall")
	form.Set("st.call.ft", "CALL_AUDIO")
	form.Set("st.call.dir", "OUTGOING")
	form.Set("st.call.opponentId", fmt.Sprintf("%d", opponentID))
	if gwtRequested != "" {
		form.Set("gwt.requested", gwtRequested)
	}

	body, code, err := c.doForm(http.MethodPost, "https://ok.ru/dk", form)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("VideoChatCall HTTP %d: %s", code, truncate(body, 300))
	}

	html := string(body)
	if !strings.Contains(html, "data-module=\"call2\"") && !strings.Contains(html, "VideoChat") {
		return "", fmt.Errorf("VideoChatCall: unexpected HTML: %s", truncate(body, 400))
	}
	logInfo("VideoChatCall ok opponent=%d htmlLen=%d", opponentID, len(html))

	// Best-effort: look for UUID-looking conversation id in HTML/JSON.
	re := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	if m := re.FindString(html); m != "" {
		return m, nil
	}
	// Caller must watch calls API / next HAR for vcid if not embedded.
	return "", nil
}


// AcceptIncomingVideoChatCall opens the OK call UI for an already-ringing call.
//
// Capture (exit / callee side):
//
//	https://ok.ru/dk
//	  cmd=VideoChatCall
//	  st.call.ft=CALL_VIDEO|CALL_AUDIO
//	  st.call.dir=INCOMING
//	  st.call.conversationId=<uuid>   // already known
//	  st.call.opponentId=<caller uid>
//	  gwt.requested=<...>
//
// HTML: data-conversation-id, data-direction=INCOMING, data-module=call2
// After this → vchat.getConversationParams(conversationId) → ws2 tgt=accept
func (c *OkClient) AcceptIncomingVideoChatCall(conversationID string, opponentID int64, gwtRequested string, video bool) error {
	ft := "CALL_AUDIO"
	if video {
		ft = "CALL_VIDEO"
	}
	form := url.Values{}
	form.Set("cmd", "VideoChatCall")
	form.Set("st.call.ft", ft)
	form.Set("st.call.dir", "INCOMING")
	form.Set("st.call.conversationId", conversationID)
	form.Set("st.call.opponentId", fmt.Sprintf("%d", opponentID))
	if gwtRequested != "" {
		form.Set("gwt.requested", gwtRequested)
	}
	body, code, err := c.doForm(http.MethodPost, "https://ok.ru/dk", form)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("VideoChatCall INCOMING HTTP %d: %s", code, truncate(body, 300))
	}
	html := string(body)
	if !strings.Contains(html, conversationID) && !strings.Contains(html, "INCOMING") {
		logInfo("VideoChatCall INCOMING: response may be partial htmlLen=%d", len(html))
	} else {
		logInfo("VideoChatCall INCOMING ok conv=%s opponent=%d", conversationID, opponentID)
	}
	return nil
}
