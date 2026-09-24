package oneme

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/pierrec/lz4/v4"
)

func decodeCallDetails(vcp string) (string, error) {
	if len(vcp) < 4 {
		return "", fmt.Errorf("vcp too short")
	}
	var size int
	fmt.Sscanf(vcp[:3], "%d", &size)
	if size < 0 {
		return "", fmt.Errorf("invalid vcp size prefix %q", vcp[:3])
	}
	decoded, err := base64.StdEncoding.DecodeString(vcp[4:])
	if err != nil {
		return "", err
	}
	decompressed := make([]byte, size)
	n, err := lz4.UncompressBlock(decoded, decompressed)
	if err != nil {
		return "", err
	}
	return string(decompressed[:n]), nil
}

func craftEndpoint(convID, jsonConfig string) string {
	var config WebRTCConfig
	json.Unmarshal([]byte(jsonConfig), &config)
	baseURL := config.WebsocketEndpoint
	if len(baseURL) > 4 {
		baseURL = baseURL[:len(baseURL)-4]
	}
	userID := config.TurnUsername
	for i := len(config.TurnUsername) - 1; i >= 0; i-- {
		if config.TurnUsername[i] == ':' {
			userID = config.TurnUsername[i+1:]
			break
		}
	}
	return fmt.Sprintf("%s/ws2?userId=%s&entityType=USER&deviceIdx=0&conversationId=%s&token=%s&platform=WEB&appVersion=1.1&version=5&device=browser&capabilities=2A03F&clientType=ONE_ME&tgt=accept",
		baseURL, userID, convID, config.Token)
}
