package transport

import (
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// frameDemux splits one carrier between the two layerings OpenFlux has
// (see PROTOCOL_NEGOTIATION.md):
//
//	Session  carrier <- AES-GCM record ("OFX...") <- batch-v2 <- envelopes
//	classic  carrier <- codec frame (0x02 / 0x00 / 0x1F) <- AES-GCM <- IPv4
//
// The first byte tells them apart, so a Session and its classic fallback
// share a carrier without either seeing the other's frames. Only the
// Session side starts and stops the carrier.
type frameDemux struct {
	raw  Transport
	name string

	once      sync.Once
	mu        sync.RWMutex
	sessionCb func([]byte)
	classicCb func([]byte)
}

func newFrameDemux(raw Transport, name string) *frameDemux {
	return &frameDemux{raw: raw, name: name}
}

func (d *frameDemux) attach() {
	d.once.Do(func() { d.raw.Receive(d.dispatch) })
}

func (d *frameDemux) dispatch(p []byte) {
	if len(p) == 0 {
		return
	}
	d.mu.RLock()
	sessionCb, classicCb := d.sessionCb, d.classicCb
	d.mu.RUnlock()
	if p[0] == encryptedMagic[0] {
		if sessionCb != nil {
			sessionCb(p)
		}
		return
	}
	if classicCb != nil {
		classicCb(p)
		return
	}
	if len(p) == 1 && p[0] == 0x00 {
		return // a carrier keepalive (Volga), not a peer frame
	}
	if utils.Throttled("demux.classic."+d.name, 30*time.Second) {
		utils.Infof("[SESSION] %q: the peer sends classic-mode frames (first byte 0x%02x)%s, but this side serves Session peers only "+
			"(--negotiate, --transports, .conf transports, a wizard node): update the peer to a build on this core, give it a Session "+
			"profile, or run this side classic (--transport=X with a key), which serves both",
			d.name, p[0], classicFrameKind(p[0]))
	}
}

func classicFrameKind(b byte) string {
	switch b {
	case batchFormatVersion:
		return " - batch-v2"
	case 0x00, CompressionMarker:
		return " - legacy per-packet"
	}
	if b>>4 == 4 {
		return " - unencrypted IPv4"
	}
	return ""
}

func (d *frameDemux) side(session bool) *demuxSide {
	return &demuxSide{d: d, session: session}
}

// demuxSide is one layering's view of the carrier.
type demuxSide struct {
	d       *frameDemux
	session bool
}

func (s *demuxSide) Start() error {
	if s.session {
		return s.d.raw.Start()
	}
	return nil
}

func (s *demuxSide) Stop() error {
	if s.session {
		return s.d.raw.Stop()
	}
	return nil
}

func (s *demuxSide) Send(p []byte) error   { return s.d.raw.Send(p) }
func (s *demuxSide) IsConnected() bool     { return s.d.raw.IsConnected() }
func (s *demuxSide) Stats() TransportStats { return s.d.raw.Stats() }

func (s *demuxSide) Receive(cb func([]byte)) {
	s.d.mu.Lock()
	if s.session {
		s.d.sessionCb = cb
	} else {
		s.d.classicCb = cb
	}
	s.d.mu.Unlock()
	s.d.attach()
}
