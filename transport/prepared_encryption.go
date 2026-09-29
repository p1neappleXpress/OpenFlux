package transport

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"openflux/utils"
)

// PreparedEncryption contains private key material computed outside a
// memory-constrained process. Treat its serialized form like the shared secret:
// keep it in the shared Keychain, never in logs or VPN providerConfiguration.
// The wire KDF and AES-GCM format are unchanged.
type PreparedEncryption struct {
	secret  string
	masters map[string][]byte
}

type preparedKey struct {
	Context string `json:"context"`
	Master  []byte `json:"master"`
}

type preparedPayload struct {
	Version int           `json:"version"`
	Keys    []preparedKey `json:"keys"`
}

type preparedEnvelope struct {
	Payload json.RawMessage `json:"payload"`
	MAC     []byte          `json:"mac"`
}

// PrepareEncryption derives all contexts sequentially in the containing app.
// The MAC binds the private bundle to the current secret, so a key edit cannot
// silently reuse old material. It is not a public password verifier: the entire
// bundle contains decryption keys and must stay in protected storage.
func PrepareEncryption(secret string, contexts []string) ([]byte, error) {
	if utils.SecretChars(secret) < utils.MinSecretChars {
		return nil, fmt.Errorf("encryption secret must contain at least %d characters", utils.MinSecretChars)
	}
	if len(contexts) == 0 || len(contexts) > maxContextCandidates {
		return nil, errors.New("invalid prepared encryption context count")
	}
	payload := preparedPayload{Version: 1, Keys: make([]preparedKey, 0, len(contexts))}
	seen := make(map[string]bool, len(contexts))
	for _, context := range contexts {
		if context == "" || seen[context] {
			return nil, errors.New("empty or duplicate prepared encryption context")
		}
		seen[context] = true
		master, err := deriveMasterKey(secret, context)
		if err != nil {
			return nil, err
		}
		payload.Keys = append(payload.Keys, preparedKey{Context: context, Master: master})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("OpenFlux prepared encryption v1\x00"))
	mac.Write(data)
	return json.Marshal(preparedEnvelope{Payload: data, MAC: mac.Sum(nil)})
}

// ParsePreparedEncryption validates a Keychain bundle without running scrypt.
// Missing, stale or corrupt material fails closed; the extension must ask the
// user to reconnect from the containing app instead of deriving keys itself.
func ParsePreparedEncryption(data []byte, secret string, contexts []string) (*PreparedEncryption, error) {
	if len(data) == 0 || len(data) > 64<<10 || utils.SecretChars(secret) < utils.MinSecretChars {
		return nil, errors.New("missing or invalid prepared encryption; reconnect from the app")
	}
	var envelope preparedEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, errors.New("invalid prepared encryption envelope")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("OpenFlux prepared encryption v1\x00"))
	mac.Write(envelope.Payload)
	if !hmac.Equal(envelope.MAC, mac.Sum(nil)) {
		return nil, errors.New("prepared encryption no longer matches the key; reconnect from the app")
	}
	var payload preparedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, errors.New("invalid prepared encryption payload")
	}
	if payload.Version != 1 || len(payload.Keys) == 0 || len(payload.Keys) > maxContextCandidates {
		return nil, errors.New("unsupported prepared encryption payload")
	}
	prepared := &PreparedEncryption{secret: secret, masters: make(map[string][]byte, len(payload.Keys))}
	actual := make([]string, 0, len(payload.Keys))
	for _, key := range payload.Keys {
		if key.Context == "" || len(key.Master) != 32 || prepared.masters[key.Context] != nil {
			return nil, errors.New("invalid prepared encryption key")
		}
		prepared.masters[key.Context] = key.Master
		actual = append(actual, key.Context)
	}
	if !slices.Equal(actual, contexts) {
		return nil, errors.New("prepared encryption no longer matches the profile; reconnect from the app")
	}
	return prepared, nil
}

// SetPreparedEncryption supplies keys for all links, including links added by
// the manager after startup. Call before adding any carriers. Unknown contexts
// are rejected; they never cause an scrypt allocation in this session.
func (s *Session) SetPreparedEncryption(prepared *PreparedEncryption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prepared == nil || len(prepared.masters) == 0 {
		return errors.New("session: prepared encryption is empty")
	}
	if s.started || len(s.links) != 0 {
		return errors.New("session: set prepared encryption before adding transports")
	}
	s.prepared = prepared
	return nil
}
