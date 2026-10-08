package yandex

// Publisher SDP builder.
//
// What the SFU needs to accept a video track, and what the previous code got
// wrong:
//
//  1. Direction. Pion's AddTrack leaves the transceiver sendrecv, so the offer
//     carried "a=sendrecv" on the video m-line. A publisher never receives, and
//     the screen-share content attribute is only ever added to a sendonly
//     m-line. The old addContentAndSimulcast() looked for "a=sendonly" to anchor
//     "a=content:speaker,main", so it never fired - the offer had neither
//     sendonly nor content.
//
//  2. Simulcast SSRCs. The old code appended
//         a=rid:low send / a=rid:med send / a=rid:hi send
//         a=simulcast:send low;med;hi
//     while leaving Pion's single "a=ssrc:" line in place. The SFU answered
//     "a=simulcast:recv low;med;hi" - so it was in simulcast-receive mode with
//     no rid -> ssrc mapping at all, and every packet was unroutable. When
//     simulcast is negotiated the three SSRCs have to be declared, either in
//     the SDP with a=ssrc-group:SIM, or dynamically via the RID header
//     extension. The SFU's answer contained no a=extmap at all, so the
//     extension path was not available to it either.
//
//  3. Header extensions. MediaEngine.RegisterDefaultCodecs() registers codecs
//     only. With no extension registered, Pion emits no a=extmap lines, so the
//     offer never mentioned sdes:rtp-stream-id and the SFU had no way to learn
//     an SSRC dynamically.
//
// buildPublisherSDP takes Pion's local SDP, forces sendonly on the video
// m-line, injects the content attribute, and - in simulcast mode - replaces the
// single a=ssrc block with three declared SSRCs plus a=ssrc-group:SIM.

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/pion/webrtc/v4"
)

// publishMode selects what the publisher offers for its video track.
type publishMode int

const (
	// modeSingle offers one SSRC, no rid/simulcast. This is exactly what a
	// browser does for a camera, and what a browser does for getDisplayMedia
	// when simulcast is off.
	modeSingle publishMode = iota
	// modeSimulcast offers three SSRCs with a=simulcast:send low;med;hi and
	// a=ssrc-group:SIM, and stamps each layer with its RID.
	modeSimulcast
	// modeDual offers single-layer SDP but also transmits the frame on the
	// three declared simulcast SSRCs. Useful only when probing an SFU whose
	// expectations are unknown.
	modeDual
)

func (m publishMode) String() string {
	switch m {
	case modeSimulcast:
		return "simulcast"
	case modeDual:
		return "dual"
	default:
		return "single"
	}
}

// parsePublishMode reads TELEMOST_PUBLISH ("single" | "simulcast" | "dual").
func parsePublishMode(v string) publishMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "simulcast", "sim", "static":
		return modeSimulcast
	case "dual", "both":
		return modeDual
	default:
		return modeSingle
	}
}

// simulcastRIDs is the layer order Telemost expects, lowest first.
var simulcastRIDs = []string{"low", "med", "hi"}

// envOrDefault is os.Getenv with a fallback, for logging.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// publishKind selects which kind of outbound video track this participant
// claims. Screen sharing is single-occupant per room: the SFU pins the share
// slot to one participant at a time, so if both tunnel ends advertise
// sharing, only one of them ever gets forwarded and the other is silently
// starved. Camera video has no such limit.
type publishKind int

const (
	// kindSharing claims the room's single screen-share slot.
	kindSharing publishKind = iota
	// kindCamera advertises an ordinary camera track, which every
	// participant may hold simultaneously.
	kindCamera
	// kindNone publishes nothing - subscriber only.
	kindNone
)

// parsePublishKind reads TELEMOST_PUBLISH_KIND ("sharing" | "camera" | "none").
func parsePublishKind(v string) publishKind {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "camera", "cam":
		return kindCamera
	case "none", "off", "subscribe", "sub":
		return kindNone
	default:
		return kindSharing
	}
}

// parseShareCodec reads TELEMOST_CODEC ("vp8" | "vp9"). VP8 is the long-
// standing default; VP9 is there to test whether the SFU's forwarding
// behaves differently by declared codec (see canvas_vp9.go).
func parseShareCodec(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "vp9":
		return webrtc.MimeTypeVP9
	case "h264":
		return webrtc.MimeTypeH264
	default:
		return webrtc.MimeTypeVP8
	}
}

// parseShareCarrier reads TELEMOST_CARRIER ("video" | "audio"): which
// track tunnel data actually rides on (see the shareCarrier field comment
// on TelemostTransport and canvas_audio.go).
func parseShareCarrier(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "audio":
		return "audio"
	default:
		return "video"
	}
}

// sdpPayloadTypeFor finds the payload type our own offer assigned to
// codecName (e.g. "VP8", "VP9") on a 90kHz m=video line, by reading the
// a=rtpmap line Pion already wrote rather than assuming a fixed number -
// which payload type a given codec lands on depends on registration order
// and what else is on that m-line, not on the codec itself.
func sdpPayloadTypeFor(sdpStr, codecName string) (int, bool) {
	needle := strings.ToUpper(codecName + "/90000")
	for _, line := range strings.Split(sdpStr, "\r\n") {
		if !strings.HasPrefix(line, "a=rtpmap:") {
			continue
		}
		rest := strings.TrimPrefix(line, "a=rtpmap:")
		sep := strings.IndexByte(rest, ' ')
		if sep < 0 {
			continue
		}
		ptStr, desc := rest[:sep], rest[sep+1:]
		if strings.ToUpper(desc) != needle {
			continue
		}
		pt, err := strconv.Atoi(ptStr)
		if err != nil {
			continue
		}
		return pt, true
	}
	return 0, false
}

// trackKindLabel is the "kind" field the SFU expects in
// updatePublisherTrackDescription. The real client's MediaTrackKind enum is
// {UNSPECIFIED, UNKNOWN, AUDIO, VIDEO, DISPLAY_AUDIO, DISPLAY_VIDEO} - there is
// no CAMERA member, a camera is an ordinary VIDEO track.
func (k publishKind) trackKindLabel() string {
	if k == kindCamera {
		return "VIDEO"
	}
	return "DISPLAY_VIDEO"
}

// sdpLabel is the human-readable track label.
func (k publishKind) sdpLabel() string {
	if k == kindCamera {
		return "Camera"
	}
	return "Primary Monitor"
}

// groupID is the track group. The client puts audio and camera in group 1 and
// both sharing tracks in group 2.
func (k publishKind) groupID() int {
	if k == kindCamera {
		return 1
	}
	return 2
}

// publishes reports whether this participant should create a publisher PC.
func (k publishKind) publishes() bool { return k != kindNone }

// sendsVideo reports the sendVideo capability to advertise in updateMe.
func (k publishKind) sendsVideo() bool { return k == kindCamera }

// sendsSharing reports the sendSharing capability to advertise in updateMe.
func (k publishKind) sendsSharing() bool { return k == kindSharing }

// ssrcRe matches "a=ssrc:<decimal> ...".
var ssrcRe = regexp.MustCompile(`^a=ssrc:(\d+)(?:\s+(.*))?$`)

// extmapRe matches "a=extmap:<id>[/<dirs>] <uri>".
var extmapRe = regexp.MustCompile(`^a=extmap:(\d+)(?:/\S+)?\s+(\S+)\s*$`)

// sdpInfo is everything we need to know about the video m-line Pion produced.
type sdpInfo struct {
	videoMid  string
	boundSSRC uint32
	cname     string
	streamID  string
	trackID   string
	ridExtID  int
}

// inspectVideoSDP pulls the negotiated SSRC, msid and the negotiated
// sdes:rtp-stream-id extension id out of Pion's local description.
func inspectVideoSDP(sdpStr string) sdpInfo {
	info := sdpInfo{ridExtID: -1}
	inVideo := false
	for _, l := range strings.Split(sdpStr, "\r\n") {
		if strings.HasPrefix(l, "m=video") {
			inVideo = true
			continue
		}
		if strings.HasPrefix(l, "m=") {
			inVideo = false
			continue
		}
		// Everything below must be gated on inVideo. The audio m-line comes
		// first in a Unified Plan offer and carries its own a=ssrc lines; a
		// stray match there used to hand us the *audio* SSRC, and every RTP
		// packet then went out on an SSRC the SFU had mapped to nothing.
		if !inVideo {
			continue
		}
		if m := extmapRe.FindStringSubmatch(l); m != nil {
			if m[2] == extURIRID {
				if id, err := strconv.Atoi(m[1]); err == nil {
					info.ridExtID = id
				}
			}
			continue
		}
		if m := ssrcRe.FindStringSubmatch(l); m != nil {
			ssrc, err := strconv.ParseUint(m[1], 10, 32)
			if err != nil || ssrc == 0 {
				continue
			}
			if info.boundSSRC == 0 {
				info.boundSSRC = uint32(ssrc)
			}
			if info.cname == "" && strings.HasPrefix(m[2], "cname:") {
				info.cname = strings.TrimPrefix(m[2], "cname:")
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "a=mid:"):
			info.videoMid = strings.TrimPrefix(l, "a=mid:")
		case strings.HasPrefix(l, "a=msid:"):
			f := strings.Fields(strings.TrimPrefix(l, "a=msid:"))
			if len(f) == 2 {
				info.streamID, info.trackID = f[0], f[1]
			}
		}
	}
	return info
}

// extraSSRC derives one more SSRC for a simulcast layer. It must be non-zero,
// distinct from the others, and must not collide with an SSRC Pion already put
// in the SDP.
func extraSSRC(existing map[uint32]bool) uint32 {
	for {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Sprintf("telemost: no entropy for ssrc: %v", err))
		}
		v := binary.BigEndian.Uint32(b[:])
		// 0 and 0xFFFFFFFF are reserved by RFC 3550.
		if v == 0 || v == 0xFFFFFFFF {
			continue
		}
		if existing[v] {
			continue
		}
		existing[v] = true
		return v
	}
}

// videoTrackPlan is the result of planning the offer: the SSRCs each RID rides
// on, and the SDP that declares them.
//
// It is read by sendBatch on the data path and updated by adoptBoundSSRC from
// the signaling goroutine, so every accessor takes the lock.
type videoTrackPlan struct {
	info    sdpInfo
	mode    publishMode
	ridSSRC map[string]uint32
	sdp     string

	mu sync.RWMutex
}

// planVideoTrack builds the SDP we hand to the SFU and the SSRC -> RID mapping
// sendBatch must use.
//
// baseSSRC is the SSRC Pion negotiated; it is always the "primary" stream, so
// in single mode it is the only one, and in simulcast mode it carries the
// lowest layer.
func planVideoTrack(sdpStr string, mode publishMode) (*videoTrackPlan, error) {
	info := inspectVideoSDP(sdpStr)
	if info.boundSSRC == 0 {
		return nil, fmt.Errorf("telemost: no a=ssrc in the local video m-line; Pion never generated one")
	}
	used := map[uint32]bool{info.boundSSRC: true}

	plan := &videoTrackPlan{info: info, mode: mode, ridSSRC: map[string]uint32{}}
	plan.ridSSRC[simulcastRIDs[0]] = info.boundSSRC

	if mode == modeSimulcast || mode == modeDual {
		for _, rid := range simulcastRIDs[1:] {
			plan.ridSSRC[rid] = extraSSRC(used)
		}
	}

	plan.sdp = rewriteVideoSDP(sdpStr, plan)
	return plan, nil
}

// adoptBoundSSRC reconciles the plan with the SSRC Pion actually bound in
// Bind(), which is the authoritative one: it is what SRTP was keyed on, and it
// is what every packet must carry. Returns true when it disagreed with the
// value parsed out of the offer we already sent - a real bug, because then the
// SFU is mapping a different SSRC than the one we transmit on.
func (p *videoTrackPlan) adoptBoundSSRC(ssrc uint32) bool {
	if ssrc == 0 || p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ssrc == p.info.boundSSRC {
		return false
	}
	p.info.boundSSRC = ssrc
	p.ridSSRC[simulcastRIDs[0]] = ssrc
	return true
}

// rewriteVideoSDP applies the publisher fixes to the video m-line. Everything
// outside the video m-line is passed through untouched.
func rewriteVideoSDP(sdpStr string, plan *videoTrackPlan) string {
	lines := strings.Split(sdpStr, "\r\n")
	out := make([]string, 0, len(lines)+32)

	inVideo := false
	inserted := false
	dirFixed := false

	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "m=video"):
			inVideo = true
			inserted = false
			dirFixed = false
			out = append(out, l)
			continue
		case strings.HasPrefix(l, "m="):
			inVideo = false
			out = append(out, l)
			continue
		}

		if !inVideo {
			out = append(out, l)
			continue
		}

		// Simulcast is rebuilt from scratch. In single/dual mode the offer
		// carries no a=rid / a=simulcast / a=ssrc-group, so Pion's own a=ssrc
		// lines are the only declaration and they must survive.
		if strings.HasPrefix(l, "a=rid:") || strings.HasPrefix(l, "a=simulcast:") ||
			strings.HasPrefix(l, "a=ssrc-group:") || strings.HasPrefix(l, "a=content:") {
			continue
		}
		if plan.mode == modeSimulcast && strings.HasPrefix(l, "a=ssrc:") {
			continue
		}

		// Force direction. Default sendonly for screen-share compatibility.
		// TELEMOST_PUBLISHER_DIR=sendrecv enables bidirectional tunnel on one track.
		pubDir := envOrDefault("TELEMOST_PUBLISHER_DIR", "sendonly")
		if l == "a=sendrecv" || l == "a=recvonly" || l == "a=inactive" || l == "a=sendonly" {
			if !dirFixed {
				out = append(out, "a="+pubDir)
				// content attribute for sendonly/sendrecv (screen share).
				// Without it, SFU may not forward the stream at all.
				if pubDir != "recvonly" {
					out = append(out, "a=content:speaker,main")
				}
				dirFixed = true
			}
			continue
		}

		// The rid/simulcast/ssrc block goes immediately before a=msid, which is
		// where Chrome puts it (after the last a=rtcp-fb / a=extmap).
		if !inserted && l == "a=msid:" && plan.mode == modeSimulcast {
			out = append(out, plan.ssrcBlock()...)
			inserted = true
		}

		out = append(out, l)
	}

	// Video m-line with no a=msid at all: flush the block right after the
	// direction attribute.
	if inVideo && plan.mode == modeSimulcast && !inserted {
		out = append(out, plan.ssrcBlock()...)
	}
	return strings.Join(out, "\r\n")
}

// ssrcBlock renders the simulcast declaration. Order follows Chrome:
//
//	a=simulcast:send low;med;hi
//	a=rid:low send
//	a=rid:med send
//	a=rid:hi send
//	a=ssrc-group:SIM <hi> <med> <low>
//	a=ssrc:<ssrc> cname:...
//	a=ssrc:<ssrc> msid:...
//	...
func (p *videoTrackPlan) ssrcBlock() []string {
	block := []string{
		"a=simulcast:send " + strings.Join(simulcastRIDs, ";"),
	}
	for _, rid := range simulcastRIDs {
		block = append(block, "a=rid:"+rid+" send")
	}
	// SIM lists the layers best-first: hi, med, low.
	block = append(block, fmt.Sprintf("a=ssrc-group:SIM %d %d %d",
		p.ridSSRC[simulcastRIDs[2]], p.ridSSRC[simulcastRIDs[1]], p.ridSSRC[simulcastRIDs[0]]))
	for _, rid := range simulcastRIDs {
		ssrc := p.ridSSRC[rid]
		block = append(block, fmt.Sprintf("a=ssrc:%d cname:%s", ssrc, p.info.cname))
		block = append(block, fmt.Sprintf("a=ssrc:%d msid:%s %s", ssrc, p.info.streamID, p.info.trackID))
		block = append(block, fmt.Sprintf("a=ssrc:%d mslabel:%s", ssrc, p.info.streamID))
		block = append(block, fmt.Sprintf("a=ssrc:%d label:%s", ssrc, p.info.trackID))
	}
	return block
}

// Layers returns the SSRC/RID pairs sendBatch must emit, in the order a
// receiver will see them.
func (p *videoTrackPlan) Layers() []telemostLayer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	switch p.mode {
	case modeSimulcast:
		out := make([]telemostLayer, 0, len(simulcastRIDs))
		for _, rid := range simulcastRIDs {
			out = append(out, telemostLayer{ssrc: p.ridSSRC[rid], rid: rid})
		}
		return out
	case modeDual:
		out := []telemostLayer{{ssrc: p.ridSSRC[simulcastRIDs[0]], rid: ""}}
		for _, rid := range simulcastRIDs {
			out = append(out, telemostLayer{ssrc: p.ridSSRC[rid], rid: rid})
		}
		return out
	default:
		return []telemostLayer{{ssrc: p.info.boundSSRC, rid: ""}}
	}
}

// telemostLayer is one outgoing RTP packet's identity: which SSRC it rides on
// and, if the offer declared simulcast, which RID it announces.
type telemostLayer struct {
	ssrc uint32
	rid  string
}
