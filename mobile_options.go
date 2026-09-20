package main

import (
	"errors"
	"openflux/internal/transportstack"
)

// Shared by both C bridges. V1 callers explicitly select legacy/no encryption.
func mobileOptions(kind, url, token, uid, codec, secret string) (transportstack.Options, error) {
	if kind == "" {
		kind = "yandex"
	}
	if codec == "" {
		codec = transportstack.Batched
	}
	switch kind {
	case "yandex", "vyandex", "oneme":
	default:
		return transportstack.Options{}, errors.New("unsupported mobile transport")
	}
	return transportstack.Options{Transport: kind, URL: url, MAXToken: token, MAXUID: uid,
		Codec: codec, EncryptionSecret: secret, ExitNode: false, Mobile: true}, nil
}
