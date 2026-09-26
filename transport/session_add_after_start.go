package transport

import (
	"fmt"

	"openflux/utils"
)

// AddTransportPostStart is the runtime version of AddTransport: it is safe
// to call after Start() has completed. The session is already ready, so no
// handshake is performed; the new transport simply becomes available for
// flow-hash routing.
//
// This is used by TransportManager when the exit node is asked to bring up
// an additional transport via SubtypeTransportStart.
func (s *Session) AddTransportPostStart(name string, raw Transport, secret, context string, priority int) error {
	if raw == nil {
		return fmt.Errorf("session: nil transport")
	}
	if name == "" {
		return fmt.Errorf("session: empty transport name")
	}

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return fmt.Errorf("session: AddTransportPostStart requires Start; use AddTransport instead")
	}
	if s.stopped {
		s.mu.Unlock()
		return fmt.Errorf("session: stopped")
	}
	if _, exists := s.links[name]; exists {
		s.mu.Unlock()
		return fmt.Errorf("session: transport %q already added", name)
	}
	s.mu.Unlock()

	enc, err := NewEncryptedTransport(raw, secret, context, s.exit)
	if err != nil {
		return fmt.Errorf("session: wrap %q: %w", name, err)
	}
	bat := NewBatchedTransport(enc)

	// bat.Start starts raw through the encryption layer.
	if err := bat.Start(); err != nil {
		return fmt.Errorf("session: transport %q start: %w", name, err)
	}

	link := &transportLink{
		name:      name,
		raw:       raw,
		encrypted: enc,
		batched:   bat,
		priority:  priority,
		started:   true,
	}
	// Same receive path as the bootstrap transports: everything that
	// arrives on this link goes through Session.receive.
	bat.Receive(func(p []byte) { s.receive(link, p) })

	s.mu.Lock()
	s.links[name] = link
	s.order = insertByPriority(s.order, name, priority, s.links)
	s.mu.Unlock()

	utils.Debugf("[SESSION] transport %q added post-start (priority=%d)", name, priority)
	return nil
}
