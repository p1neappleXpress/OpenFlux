package phpbox

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/network"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// The stream mode logs by the same rules as the packet modes (see utils/logging.go):
//
//	-d   (1)  one line per mux frame, "->" towards the internet, "<-" back to the device:
//	            [STREAM] -> 526 bytes - stream 7 DATA
//	            [STREAM] -> 28 bytes - stream 7 OPEN api.ipify.org:443
//	-dd  (2)  plus operational logs (Debugf): streams opening and closing, the carrier
//	          being busy or gone, sends given up on
//	-ddd (3)  plus a hexdump of each DATA payload (what the app wrote or was sent; TLS
//	          records are ciphertext here, plain HTTP is not - the same as a packet dump)
//
// The byte count is the frame on the wire (header + payload). Destination hosts
// show at -d the way destination IPs do in packet lines. --sensitive keeps its
// meaning (key material); this mode has none of its own, it carries no keys.
const logTag = "STREAM"

func frameName(t byte) string {
	switch t {
	case FrameOpen:
		return "OPEN"
	case FrameData:
		return "DATA"
	case FrameClose:
		return "CLOSE"
	case FrameOpenOK:
		return "OPEN_OK"
	case FrameOpenErr:
		return "OPEN_ERR"
	case FrameAck:
		return "ACK"
	}
	return fmt.Sprintf("type%d", t)
}

// logFrame logs f at level 1 (-d) and, for DATA at level 3 (-ddd), its hexdump.
func logFrame(dir network.PacketDirection, f Frame) {
	if utils.Level() < utils.LevelPackets {
		return
	}
	extra := ""
	switch f.Type {
	case FrameOpen, FrameOpenErr:
		extra = " " + strings.ReplaceAll(string(f.Payload), "\x00", " +")
	case FrameAck:
		if len(f.Payload) == 4 {
			extra = fmt.Sprintf(" consumed=%d", binary.BigEndian.Uint32(f.Payload))
		}
	}
	utils.Packetf("[%s] %s%d bytes - stream %d %s%s", logTag, dir.Arrow(), headerLen+len(f.Payload), f.StreamID, frameName(f.Type), extra)
	if f.Type == FrameData && utils.IsVerbose() {
		utils.Packetf("[%s] hexdump:\n%s", logTag, hex.Dump(f.Payload))
	}
}
