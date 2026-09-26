package control

import "encoding/json"

// TransportConfig describes one transport that the exit node should bring up.
// It travels inside SubtypeTransportStart / SubtypeTransportStop payloads.
//
// Name is a local identifier chosen by the client (e.g. "yandex-1",
// "direct-main"). Type selects the concrete transport implementation.
// URL is passed through to transports that need a document/room URL.
// Params is a free-form bag for the rest (tokens, room lists, dial
// addresses, etc.).
type TransportConfig struct {
	Name   string                 `json:"name"`
	Type   string                 `json:"type"`
	URL    string                 `json:"url,omitempty"`
	Params map[string]interface{} `json:"params,omitempty"`
}

// Encode serializes the config to JSON.
func (c *TransportConfig) Encode() ([]byte, error) {
	return json.Marshal(c)
}

// DecodeTransportConfig parses a config from a control payload.
func DecodeTransportConfig(b []byte) (*TransportConfig, error) {
	if len(b) == 0 {
		return &TransportConfig{}, nil
	}
	var out TransportConfig
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TransportStatus is the exit node's report about one transport.
type TransportStatus struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

// TransportStatusList is the payload for SubtypeTransportStatus / SubtypeTransportList.
type TransportStatusList struct {
	Transports []TransportStatus `json:"transports"`
}

// Encode serializes the status list to JSON.
func (s *TransportStatusList) Encode() ([]byte, error) {
	return json.Marshal(s)
}

// DecodeTransportStatusList parses a status list payload.
func DecodeTransportStatusList(b []byte) (*TransportStatusList, error) {
	if len(b) == 0 {
		return &TransportStatusList{}, nil
	}
	var out TransportStatusList
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
