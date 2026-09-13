package fluxcore

import (
	"encoding/binary"
	"sync"
	"time"
)

// Frame types. MetricsProbe multiplexes tiny control frames alongside real
// traffic on the *same* carrier, so it measures the path the user actually
// uses — not a separate test connection that might behave differently.
const (
	frameData byte = 0x00
	framePing byte = 0x01
	framePong byte = 0x02
)

// Link is the minimal surface MetricsProbe needs from a transport: the ability
// to send bytes. The real OpenFlux transport.Transport already satisfies this
// (it has Send([]byte) error), so the adapter is a one-liner.
type Link interface {
	Send([]byte) error
}

// MetricsProbe wraps a Link and, like CompressedTransport, is a decorator: it
// frames outgoing data, injects periodic ping frames, answers pings with
// pongs, and turns pongs into RTT/loss samples. The core stays untouched.
type MetricsProbe struct {
	link    Link
	sampler *Sampler

	mu       sync.Mutex
	seq      uint32
	inflight map[uint32]time.Time

	now func() time.Time
}

// NewMetricsProbe builds a probe over the given link, feeding the given sampler.
func NewMetricsProbe(link Link, s *Sampler) *MetricsProbe {
	return newMetricsProbe(link, s, time.Now)
}

func newMetricsProbe(link Link, s *Sampler, now func() time.Time) *MetricsProbe {
	return &MetricsProbe{
		link:     link,
		sampler:  s,
		inflight: map[uint32]time.Time{},
		now:      now,
	}
}

// SendData frames application bytes and sends them. A queue-full / send error
// is recorded as loss — exactly the Yandex "write queue full" case the brain
// was previously blind to.
func (p *MetricsProbe) SendData(payload []byte) error {
	frame := make([]byte, 1+len(payload))
	frame[0] = frameData
	copy(frame[1:], payload)
	if err := p.link.Send(frame); err != nil {
		p.sampler.ObserveLoss()
		p.sampler.SetError(err.Error())
		return err
	}
	p.sampler.RecordSend(len(payload))
	return nil
}

// Ping sends one ping frame and remembers when, so a later pong yields RTT.
func (p *MetricsProbe) Ping() error {
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.inflight[seq] = p.now()
	p.mu.Unlock()

	frame := make([]byte, 5)
	frame[0] = framePing
	binary.BigEndian.PutUint32(frame[1:], seq)
	if err := p.link.Send(frame); err != nil {
		p.mu.Lock()
		delete(p.inflight, seq)
		p.mu.Unlock()
		p.sampler.ObserveLoss()
		return err
	}
	return nil
}

// Inbound demultiplexes a received frame. It returns the application payload
// and true when the frame is real data; control frames are handled internally
// and return (nil, false).
func (p *MetricsProbe) Inbound(raw []byte) (payload []byte, isData bool) {
	if len(raw) == 0 {
		return nil, false
	}
	switch raw[0] {
	case frameData:
		body := raw[1:]
		p.sampler.RecordRecv(len(body))
		return body, true

	case framePing:
		if len(raw) >= 5 {
			pong := make([]byte, 5)
			pong[0] = framePong
			copy(pong[1:], raw[1:5])
			_ = p.link.Send(pong) // best-effort reply
		}
		return nil, false

	case framePong:
		if len(raw) >= 5 {
			seq := binary.BigEndian.Uint32(raw[1:5])
			p.mu.Lock()
			sent, ok := p.inflight[seq]
			delete(p.inflight, seq)
			p.mu.Unlock()
			if ok {
				p.sampler.ObserveRTT(p.now().Sub(sent))
			}
		}
		return nil, false
	}
	return nil, false
}

// SweepTimeouts marks any ping older than `timeout` as lost and drops it. The
// brain calls this on a cadence so a silent carrier shows up as loss, not just
// as stale RTT.
func (p *MetricsProbe) SweepTimeouts(timeout time.Duration) {
	cutoff := p.now().Add(-timeout)
	p.mu.Lock()
	var expired []uint32
	for seq, t := range p.inflight {
		if t.Before(cutoff) {
			expired = append(expired, seq)
		}
	}
	for _, seq := range expired {
		delete(p.inflight, seq)
	}
	p.mu.Unlock()

	for range expired {
		p.sampler.ObserveLoss()
	}
}
