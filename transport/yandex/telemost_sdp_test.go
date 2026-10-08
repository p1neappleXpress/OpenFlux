package yandex

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// pionOffer builds a real offer the same way initPublisherPC does, so the tests
// below run against the exact SDP shape Pion produces.
func pionOffer(t *testing.T, direction webrtc.RTPTransceiverDirection) string {
	t.Helper()
	me, err := newTelemostMediaEngine()
	if err != nil {
		t.Fatalf("media engine: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pc: %v", err)
	}
	defer func() { _ = pc.Close() }()

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		t.Fatalf("audio: %v", err)
	}
	tr, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: direction})
	if err != nil {
		t.Fatalf("video: %v", err)
	}
	if err := tr.Sender().ReplaceTrack(NewTelemostTrack(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		webrtc.RTPCodecTypeVideo,
		"openflux", "stream")); err != nil {
		t.Fatalf("replace track: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	return pc.LocalDescription().SDP
}

func videoLines(sdp string) []string {
	var out []string
	in := false
	for _, l := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(l, "m=video") {
			in = true
		} else if strings.HasPrefix(l, "m=") {
			in = false
		}
		if in {
			out = append(out, l)
		}
	}
	return out
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// TestSingleModeFixesDirection is the regression the whole transport was stuck
// on: Pion's AddTrack leaves the m-line sendrecv, the patch anchored
// a=content on "a=sendonly", so neither ever appeared.
func TestSingleModeFixesDirection(t *testing.T) {
	plan, err := planVideoTrack(pionOffer(t, webrtc.RTPTransceiverDirectionSendrecv), modeSingle)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	lines := videoLines(plan.sdp)

	if !has(lines, "a=sendonly") {
		t.Errorf("video m-line is not sendonly:\n%s", strings.Join(lines, "\n"))
	}
	if has(lines, "a=sendrecv") {
		t.Errorf("a=sendrecv survived:\n%s", strings.Join(lines, "\n"))
	}
	if !has(lines, "a=content:speaker,main") {
		t.Errorf("a=content:speaker,main missing:\n%s", strings.Join(lines, "\n"))
	}
	if !has(lines, "a=msid:"+plan.info.streamID+" "+plan.info.trackID) {
		t.Errorf("a=msid missing or changed:\n%s", strings.Join(lines, "\n"))
	}
	// Single layer: exactly one declared SSRC, no rid/simulcast.
	var ssrcLines int
	for _, l := range lines {
		if strings.HasPrefix(l, "a=ssrc:") {
			ssrcLines++
		}
		if strings.HasPrefix(l, "a=rid:") || strings.HasPrefix(l, "a=simulcast:") ||
			strings.HasPrefix(l, "a=ssrc-group:") {
			t.Errorf("single mode must not advertise simulcast: %s", l)
		}
	}
	if ssrcLines != 4 { // cname + msid + mslabel + label
		t.Errorf("expected 4 a=ssrc lines for one SSRC, got %d", ssrcLines)
	}
	if plan.info.boundSSRC == 0 {
		t.Error("boundSSRC is 0; sendBatch would emit ssrc=00000000 again")
	}
	if got := plan.Layers(); len(got) != 1 || got[0].ssrc != plan.info.boundSSRC || got[0].rid != "" {
		t.Errorf("single mode layers = %+v", got)
	}
}

// TestHeaderExtensionsRegistered is the second regression: with only
// RegisterDefaultCodecs, MediaEngine.headerExtensions is empty, so Pion emits
// no a=extmap at all and inspectVideoSDP cannot find the RID id.
func TestHeaderExtensionsRegistered(t *testing.T) {
	plan, err := planVideoTrack(pionOffer(t, webrtc.RTPTransceiverDirectionSendonly), modeSingle)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	lines := videoLines(plan.sdp)

	var found bool
	for _, l := range lines {
		if strings.Contains(l, extURIRID) {
			found = true
			if !strings.HasPrefix(l, "a=extmap:") {
				t.Errorf("RID extmap is malformed: %q", l)
			}
		}
	}
	if !found {
		t.Fatalf("no a=extmap for %s in:\n%s", extURIRID, strings.Join(lines, "\n"))
	}
	if plan.info.ridExtID < 1 {
		t.Errorf("ridExtID = %d, want the id from the offer's a=extmap", plan.info.ridExtID)
	}
	// The id has to be usable in the RFC 8285 one-byte range.
	if plan.info.ridExtID > 14 {
		t.Errorf("ridExtID = %d is outside the one-byte extension range 1..14", plan.info.ridExtID)
	}
}

// TestSimulcastDeclaresEverySSRC is the root cause: the old code advertised
// a=simulcast:send with only Pion's single a=ssrc, so the SFU answered
// a=simulcast:recv with no rid -> ssrc mapping and dropped every packet.
func TestSimulcastDeclaresEverySSRC(t *testing.T) {
	plan, err := planVideoTrack(pionOffer(t, webrtc.RTPTransceiverDirectionSendonly), modeSimulcast)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	lines := videoLines(plan.sdp)

	if !has(lines, "a=simulcast:send low;med;hi") {
		t.Errorf("a=simulcast:send missing:\n%s", strings.Join(lines, "\n"))
	}
	for _, rid := range simulcastRIDs {
		if !has(lines, "a=rid:"+rid+" send") {
			t.Errorf("a=rid:%s send missing", rid)
		}
	}

	// Every SSRC a layer rides on must be declared in the m-line.
	declared := map[uint32]bool{}
	for _, l := range lines {
		fields := strings.Fields(strings.TrimPrefix(l, "a=ssrc:"))
		if len(fields) == 0 || !strings.HasPrefix(l, "a=ssrc:") {
			continue
		}
		var v uint64
		for _, c := range fields[0] {
			if c < '0' || c > '9' {
				v = 0
				break
			}
			v = v*10 + uint64(c-'0')
		}
		declared[uint32(v)] = true
	}
	if !has(lines, "a=ssrc-group:SIM "+itoa(plan.ridSSRC["hi"])+" "+
		itoa(plan.ridSSRC["med"])+" "+itoa(plan.ridSSRC["low"])) {
		t.Errorf("a=ssrc-group:SIM does not list the layer SSRCs:\n%s", strings.Join(lines, "\n"))
	}
	layers := plan.Layers()
	if len(layers) != len(simulcastRIDs) {
		t.Fatalf("expected %d layers, got %d", len(simulcastRIDs), len(layers))
	}
	seen := map[uint32]bool{}
	for _, l := range layers {
		if l.ssrc == 0 {
			t.Errorf("layer %q has ssrc=0", l.rid)
		}
		if !declared[l.ssrc] {
			t.Errorf("layer %q rides on ssrc=%d which is not declared in the SDP", l.rid, l.ssrc)
		}
		if seen[l.ssrc] {
			t.Errorf("ssrc %d used by two layers", l.ssrc)
		}
		seen[l.ssrc] = true
		if l.rid == "" {
			t.Errorf("simulcast layer without a rid: %+v", l)
		}
	}
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// TestDualModeKeepsSingleSSRCUndeclared guards the probe mode: the plain
// copy must ride on the SSRC Pion negotiated, since that is the only one the
// SFU knows about from a non-simulcast offer.
func TestDualModeKeepsSingleSSRCUndeclared(t *testing.T) {
	plan, err := planVideoTrack(pionOffer(t, webrtc.RTPTransceiverDirectionSendonly), modeDual)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	layers := plan.Layers()
	if len(layers) != 4 {
		t.Fatalf("dual mode should emit 4 packets, got %d", len(layers))
	}
	if layers[0].rid != "" || layers[0].ssrc != plan.info.boundSSRC {
		t.Errorf("first layer = %+v, want the bound ssrc %d with no rid", layers[0], plan.info.boundSSRC)
	}
}

// declaredSSRCs returns every a=ssrc value that appears in a media section.
func declaredSSRCs(sdp string, section string) map[uint32]bool {
	out := map[uint32]bool{}
	in := false
	for _, l := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(l, "m="+section) {
			in = true
			continue
		}
		if strings.HasPrefix(l, "m=") {
			in = false
			continue
		}
		if !in {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(l, "a=ssrc:"))
		if len(fields) == 0 || !strings.HasPrefix(l, "a=ssrc:") {
			continue
		}
		var v uint64
		for _, c := range fields[0] {
			if c < '0' || c > '9' {
				v = 0
				break
			}
			v = v*10 + uint64(c-'0')
		}
		if v != 0 {
			out[uint32(v)] = true
		}
	}
	return out
}

// TestBoundSSRCComesFromTheVideoMLine is the regression that had the whole
// publisher sending on the wrong SSRC.
//
// The audio transceiver is created first, so its a=ssrc block appears first in
// the offer. inspectVideoSDP matched a=ssrc / a=extmap outside its inVideo
// guard, so boundSSRC became the *audio* SSRC (0x0ad5b611 in the live run) while
// Pion bound the video one (0x475ca63e). The SFU had mapped the audio SSRC -
// a track we never even Bind - and every video packet went out unroutable.
func TestBoundSSRCComesFromTheVideoMLine(t *testing.T) {
	sdp := pionOffer(t, webrtc.RTPTransceiverDirectionSendonly)
	plan, err := planVideoTrack(sdp, modeSingle)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	video := declaredSSRCs(sdp, "video")
	audio := declaredSSRCs(sdp, "audio")
	if len(video) != 1 || len(audio) != 1 {
		t.Fatalf("expected one SSRC per m-line, got video=%v audio=%v", video, audio)
	}
	if video[plan.info.boundSSRC] != true {
		t.Errorf("boundSSRC %d (%08x) is not declared in the video m-line; "+
			"the plan is transmitting on an SSRC the SFU never mapped",
			plan.info.boundSSRC, plan.info.boundSSRC)
	}
	if audio[plan.info.boundSSRC] {
		t.Errorf("boundSSRC %d (%08x) is the AUDIO transceiver's SSRC",
			plan.info.boundSSRC, plan.info.boundSSRC)
	}
	// The cname we stamp onto extra simulcast SSRCs must also come from video.
	if !video[cnameSSRC(sdp, plan.info.boundSSRC)] {
		t.Errorf("cname lookup disagrees with the video m-line")
	}
	if l := plan.Layers(); len(l) != 1 || l[0].ssrc != plan.info.boundSSRC {
		t.Errorf("layers = %+v, want the video ssrc %d", l, plan.info.boundSSRC)
	}
}

// cnameSSRC digs the a=ssrc:<n> cname: line for n out of the video m-line.
func cnameSSRC(sdp string, want uint32) uint32 {
	in := false
	for _, l := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(l, "m=video") {
			in = true
			continue
		}
		if strings.HasPrefix(l, "m=") {
			in = false
			continue
		}
		if in && strings.HasPrefix(l, "a=ssrc:"+itoa(want)+" cname:") {
			return want
		}
	}
	return 0
}

// TestAdoptBoundSSRCReconcilesLayers covers the case where the SFU/Pion ends up
// binding a different SSRC than the one we declared: sendBatch must switch to
// the bound value for every layer that rode on the old one.
func TestAdoptBoundSSRCReconcilesLayers(t *testing.T) {
	for _, mode := range []publishMode{modeSingle, modeDual, modeSimulcast} {
		plan, err := planVideoTrack(pionOffer(t, webrtc.RTPTransceiverDirectionSendonly), mode)
		if err != nil {
			t.Fatalf("%s plan: %v", mode, err)
		}
		old := plan.info.boundSSRC
		const rebound = uint32(0xDEADBEEF)

		if !plan.adoptBoundSSRC(rebound) {
			t.Errorf("%s: adoptBoundSSRC(%08x) reported no change", mode, rebound)
		}
		if plan.adoptBoundSSRC(rebound) {
			t.Errorf("%s: adopting the same ssrc twice reported a change", mode)
		}
		if plan.info.boundSSRC != rebound {
			t.Errorf("%s: boundSSRC = %08x, want %08x", mode, plan.info.boundSSRC, rebound)
		}
		for _, l := range plan.Layers() {
			if l.ssrc == old {
				t.Errorf("%s: layer %q still rides on the stale ssrc %08x", mode, l.rid, old)
			}
		}
	}
}
