package mobile

import (
	"encoding/json"

	"openflux/transport"
)

// ClassicSessionSpecs is the encrypted classic profile's Session representation.
// The iOS app uses the same representation to prepare keys before its extension
// starts, including direct/MAX parameters and the core's context selection.
func ClassicSessionSpecs(transportType, documentURL, maxToken, maxUID string) string {
	if transportType == "" {
		transportType = "yandex"
	}
	specs, _ := json.Marshal([]sessionSpec{{
		Name: transportType, Type: transportType, URL: documentURL, Priority: 100,
		Params: classicParams(transportType, documentURL, maxToken, maxUID),
	}})
	return string(specs)
}

// PrepareSessionKeys runs in the containing app, never in a Network Extension.
// Its result is private key material for shared Keychain storage.
func PrepareSessionKeys(specsJSON, secret string) (string, error) {
	_, context, alternates, err := parseSessionSpecs(specsJSON)
	if err != nil {
		return "", err
	}
	data, err := transport.PrepareEncryption(secret, append([]string{context}, alternates...))
	return string(data), err
}

// StartPreparedSession starts an encrypted packet client without running scrypt.
// classicCodec is empty for a Session profile, or the classic framing preference.
// A missing or stale bundle fails before creating any carrier or network socket.
func StartPreparedSession(specsJSON, secret, bundle, classicCodec string) string {
	_, context, alternates, err := parseSessionSpecs(specsJSON)
	if err != nil {
		return err.Error()
	}
	prepared, err := transport.ParsePreparedEncryption(
		[]byte(bundle), secret, append([]string{context}, alternates...),
	)
	if err != nil {
		return "ключи VPN нужно подготовить заново в приложении: " + err.Error()
	}
	return startPacket(func() (transport.Transport, error) {
		t, _, err := buildSessionWith(
			specsJSON,
			secret,
			false,
			sessionOptions{classic: classicCodec != "", codec: classicCodec, prepared: prepared},
		)
		return t, err
	})
}
