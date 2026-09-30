package control

import "encoding/json"

// CookiesPayload is the body of SubtypeCookiesRequest/Response/Offer.
//
// The map is name -> value. Domain is optional and, when set, tells the
// receiving side which host the cookies belong to; empty means "the host
// of the document this transport is currently attached to".
type CookiesPayload struct {
	// Transport names the transport the cookies belong to. Empty (older
	// peers) means the highest-priority transport that carries cookies.
	Transport string `json:"transport,omitempty"`
	// Doc is the document URL of that transport on the sender's side, so
	// the receiver can find its own carrier when the two sides name them
	// differently (an app naming carriers after their type, a .conf naming
	// sections freely). Optional; older peers ignore it.
	Doc    string            `json:"doc,omitempty"`
	Jar    map[string]string `json:"jar,omitempty"`
	Domain string            `json:"domain,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

// Encode serializes the payload to JSON.
func (c *CookiesPayload) Encode() ([]byte, error) {
	return json.Marshal(c)
}

// DecodeCookies parses a payload received on the wire.
func DecodeCookies(b []byte) (*CookiesPayload, error) {
	if len(b) == 0 {
		return &CookiesPayload{}, nil
	}
	var out CookiesPayload
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AuthRequiredPayload is the body of SubtypeAuthRequired.
type AuthRequiredPayload struct {
	Transport string `json:"transport"`
	URL       string `json:"url"`
	Reason    string `json:"reason"`
	// Doc: see CookiesPayload.Doc.
	Doc string `json:"doc,omitempty"`
}

// Encode serializes the payload to JSON.
func (a *AuthRequiredPayload) Encode() ([]byte, error) {
	return json.Marshal(a)
}

// DecodeAuthRequired parses a payload received on the wire.
func DecodeAuthRequired(b []byte) (*AuthRequiredPayload, error) {
	var out AuthRequiredPayload
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
