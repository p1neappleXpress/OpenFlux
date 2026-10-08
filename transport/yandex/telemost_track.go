package yandex

// Custom RTP sender for the Telemost publisher track.
//
// Why not webrtc.TrackLocalStaticRTP:
//
//	// track_local_static.go, writeRTP()
//	for _, b := range s.bindings {
//	    p.Header.SSRC        = uint32(b.ssrc)
//	    p.Header.PayloadType = uint8(b.payloadType)
//	    ...
//	}
//
// Pion unconditionally stamps the single SSRC it generated onto every packet
// we hand it, and there is exactly one binding per RTPSender. That makes the
// "three simulcast layers on three SSRCs" scheme impossible through
// TrackLocalStaticRTP: all three layers leave with the same SSRC and differ
// only in the RID extension, which is not a legal encoding of simulcast and is
// what the SFU drops. It also means the SSRC printed by our own debug log
// (whatever we put in pkt.Header.SSRC) is a lie - on the wire it was always the
// negotiated SSRC.
//
// TelemostTrack therefore keeps the webrtc.TrackLocalWriter that Pion hands it
// in Bind() and writes the header verbatim into it, so SSRC, payload type and
// header extensions survive untouched all the way into SRTP:
//
//	TelemostTrack -> interceptor (no-op) -> srtpWriterFuture
//	              -> srtp.WriteStreamSRTP -> Context.encryptRTP
//	              -> rtp.Header.MarshalTo   (header exactly as we built it)
//
// The SRTP context keys off the SSRC in the header we pass
// (Context.getSRTPSSRCState), so arbitrary simulcast SSRCs encrypt correctly on
// the same DTLS/SRTP session.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Header extension URIs. The three sdes ones matter for static simulcast; the
// last two are what a Chrome publisher always offers, and matching a real
// browser's extmap set keeps the SFU from treating us as a broken client.
const (
	extURIRID         = "urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id"
	extURIRepairedRID = "urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id"
	extURIMID         = "urn:ietf:params:rtp-hdrext:sdes:mid"
	extURITWCC        = "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"
	extURIABS         = "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"
)

// rfc8285OneByteHeader marks the one-byte header extension profile (0xBEDE).
// Pion's rtp.Header picks this automatically for payloads of 16 bytes or less,
// but we set it explicitly so a hand-built header cannot end up with the
// RFC3550 profile (which only permits id 0).
const rfc8285OneByteHeader = 0xBEDE

// telemostBinding is one negotiated send path: the SRTP write stream plus the
// SSRC and payload type Pion matched for it.
type telemostBinding struct {
	ssrc        webrtc.SSRC
	payloadType webrtc.PayloadType
	writer      webrtc.TrackLocalWriter
	ctxID       string
}

// TelemostTrack implements webrtc.TrackLocal with byte-level control over the
// outgoing RTP header.
type TelemostTrack struct {
	codec    webrtc.RTPCodecCapability
	kind     webrtc.RTPCodecType
	id       string
	streamID string
	cname    string

	mu       sync.RWMutex
	bindings []*telemostBinding
	bound    chan struct{}
	bindOnce sync.Once

	// RTCP Sender Report counters. Pion v3.3.6 never emits an SR for a custom
	// TrackLocal (srtpWriterFuture.Write is unreachable from TrackLocalContext),
	// but every browser does, and an SFU commonly waits for one before it
	// promotes an inbound stream to a published slot.
	pktsSent   atomic.Uint32
	octetsSent atomic.Uint32
	lastRTPTS  atomic.Uint32
	lastWallNs atomic.Int64
}

// NewTelemostTrack builds an unbound track of the given kind (video or
// audio - see canvas_audio.go for the audio carrier this enables).
func NewTelemostTrack(codec webrtc.RTPCodecCapability, kind webrtc.RTPCodecType, id, streamID string) *TelemostTrack {
	return &TelemostTrack{
		codec:    codec,
		kind:     kind,
		id:       id,
		streamID: streamID,
		cname:    "openflux-" + id[:8],
		bound:    make(chan struct{}),
	}
}

// ---- webrtc.TrackLocal ----

// Bind is called by Pion once the answer picked a codec. We remember the write
// stream and the negotiated SSRC/PT, and publish the codec back so Pion's SDP
// contains the matching a=ssrc lines.
func (t *TelemostTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	var match webrtc.RTPCodecParameters
	found := false
	for _, c := range ctx.CodecParameters() {
		if c.MimeType == t.codec.MimeType {
			match, found = c, true
			break
		}
	}
	if !found {
		return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
	}

	b := &telemostBinding{
		ssrc:        ctx.SSRC(),
		payloadType: match.PayloadType,
		writer:      ctx.WriteStream(),
		ctxID:       ctx.ID(),
	}

	t.mu.Lock()
	t.bindings = append(t.bindings, b)
	t.mu.Unlock()
	t.bindOnce.Do(func() { close(t.bound) })

	utils.Debugf("[Telemost] track bound: id=%s ssrc=%08x pt=%d",
		t.id, uint32(b.ssrc), match.PayloadType)
	return match, nil
}

// Unbind drops the binding Pion is tearing down.
func (t *TelemostTrack) Unbind(ctx webrtc.TrackLocalContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, b := range t.bindings {
		if b.ctxID == ctx.ID() {
			t.bindings = append(t.bindings[:i], t.bindings[i+1:]...)
			return nil
		}
	}
	return webrtc.ErrUnbindFailed
}

func (t *TelemostTrack) ID() string                { return t.id }
func (t *TelemostTrack) RID() string               { return "" }
func (t *TelemostTrack) StreamID() string          { return t.streamID }
func (t *TelemostTrack) Kind() webrtc.RTPCodecType { return t.kind }

// ---- sending ----

// SSRC returns the SSRC Pion negotiated for this track, or 0 before Bind.
func (t *TelemostTrack) SSRC() webrtc.SSRC {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.bindings) == 0 {
		return 0
	}
	return t.bindings[0].ssrc
}

// PayloadType returns the payload type Pion negotiated, or 0 before Bind.
func (t *TelemostTrack) PayloadType() webrtc.PayloadType {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.bindings) == 0 {
		return 0
	}
	return t.bindings[0].payloadType
}

// WriteHeader writes one RTP packet with the header exactly as given. SSRC and
// payload type in hdr are used as-is; nothing rewrites them.
//
// Pion's srtpWriterFuture returns (0, nil) when DTLS has not finished yet, so
// the returned byte count is also our readiness signal.
func (t *TelemostTrack) WriteHeader(hdr rtp.Header, payload []byte) (int, error) {
	t.mu.RLock()
	bindings := t.bindings
	t.mu.RUnlock()

	if len(bindings) == 0 {
		return 0, fmt.Errorf("telemost track: not bound")
	}
	n, err := bindings[0].writer.WriteRTP(&hdr, payload)
	if n > 0 {
		t.pktsSent.Add(1)
		t.octetsSent.Add(uint32(len(payload)))
		t.lastRTPTS.Store(hdr.Timestamp)
		t.lastWallNs.Store(time.Now().UnixNano())
	}
	return n, err
}

// CNAME is the RTCP SDES canonical name a browser advertises for its stream.
func (t *TelemostTrack) CNAME() string { return t.cname }

// SenderReport builds the periodic RTCP SR every browser sends. The SFU uses
// it to bind the SSRC to a stream and to measure our send rate.
func (t *TelemostTrack) SenderReport() *rtcp.SenderReport {
	ssrc := uint32(t.SSRC())
	if ssrc == 0 {
		return nil
	}
	return &rtcp.SenderReport{
		SSRC:        ssrc,
		NTPTime:     ntpNow(),
		RTPTime:     t.lastRTPTS.Load(),
		PacketCount: t.pktsSent.Load(),
		OctetCount:  t.octetsSent.Load(),
	}
}

// ntpNow returns the current time in the 64-bit NTP format RTCP uses:
// seconds since 1900 in the high word, binary fraction in the low word.
func ntpNow() uint64 {
	const ntpEpochOffset = 2208988800 // seconds between 1900 and 1970
	now := time.Now()
	secs := uint64(now.Unix() + ntpEpochOffset)
	frac := uint64(now.Nanosecond()) << 32 / 1e9
	return secs<<32 | frac
}

// Probe writes a single empty packet and reports whether the SRTP stream is
// live. Pion drops writes silently before DTLS completes, so a zero-length
// result means "not ready yet" rather than "sent".
func (t *TelemostTrack) Probe() bool {
	t.mu.RLock()
	bindings := t.bindings
	t.mu.RUnlock()
	if len(bindings) == 0 {
		return false
	}
	hdr := rtp.Header{
		Version:        2,
		PayloadType:    uint8(bindings[0].payloadType),
		SSRC:           uint32(bindings[0].ssrc),
		SequenceNumber: 0,
		Timestamp:      0,
	}
	n, _ := bindings[0].writer.WriteRTP(&hdr, nil)
	return n > 0
}

// WaitBound blocks until Pion has called Bind, i.e. until the answer selected a
// codec for this track.
func (t *TelemostTrack) WaitBound(d time.Duration) bool {
	select {
	case <-t.bound:
		return true
	case <-time.After(d):
		return false
	}
}
