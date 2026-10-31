package ok

import (
	"fmt"
	"net/url"
	"strings"
)

// craftPortalEndpoint builds the videowebrtc signaling URL for OK (PORTAL).
// Example from capture:
//
//	wss://videowebrtc.okcdn.ru/ws2?userId=910703757272&entityType=USER
//	  &conversationId=...&token=...&platform=WEB&appVersion=1.1&version=5
//	  &device=browser&capabilities=3F&clientType=PORTAL&tgt=start
//
// clientType MUST be PORTAL (oneme hardcodes ONE_ME).
func craftPortalEndpoint(p CallParams, tgt string) string {
	if strings.Contains(p.Endpoint, "ws2?") {
		// API already returned a full URL — only force PORTAL if missing.
		u := p.Endpoint
		if !strings.Contains(u, "clientType=") {
			u += "&clientType=PORTAL"
		}
		if !strings.Contains(u, "tgt=") {
			u += "&tgt=" + tgt
		}
		return u
	}

	base := p.Endpoint
	if base == "" {
		base = "wss://videowebrtc.okcdn.ru"
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/ws2") {
		base = strings.TrimSuffix(base, "/ws2")
	}

	q := url.Values{}
	q.Set("userId", p.UserID)
	q.Set("entityType", "USER")
	q.Set("conversationId", p.ConvID)
	q.Set("token", p.Token)
	q.Set("platform", "WEB")
	q.Set("appVersion", "1.1")
	q.Set("version", "5")
	q.Set("device", "browser")
	// OK capture used capabilities=3F; MAX/oneme uses 2A03F / 6A03F.
	q.Set("capabilities", "3F")
	q.Set("clientType", "PORTAL")
	q.Set("tgt", tgt)

	return fmt.Sprintf("%s/ws2?%s", base, q.Encode())
}
