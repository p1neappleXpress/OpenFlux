package network

import (
	"bytes"
	"strings"
	"testing"

	"openflux/utils"
)

// udpPacket is a 29-byte IPv4/UDP packet 10.10.10.2:53000 -> 8.8.8.8:53
// carrying one payload byte.
func udpPacket() []byte {
	return []byte{
		0x45, 0x00, 0x00, 0x1d, 0x00, 0x00, 0x00, 0x00, 0x40, 0x11, 0x00, 0x00,
		10, 10, 10, 2, 8, 8, 8, 8,
		0xcf, 0x08, 0x00, 0x35, 0x00, 0x09, 0x00, 0x00,
		0xab,
	}
}

func TestFormatPacketUDP(t *testing.T) {
	got := FormatPacket(DirOutbound, udpPacket())
	want := "-> 29 bytes - UDP 10.10.10.2:53000 -> 8.8.8.8:53 len=9 ttl=64"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestLogPacketLevels(t *testing.T) {
	var buf bytes.Buffer
	utils.SetOutput(&buf)
	defer utils.SetLevel(utils.LevelOff)

	for _, c := range []struct {
		level         int
		line, hexdump bool
	}{
		{utils.LevelOff, false, false},
		{utils.LevelPackets, true, false},
		{utils.LevelDebug, true, false},
		{utils.LevelHexdump, true, true},
	} {
		buf.Reset()
		utils.SetLevel(c.level)
		LogPacket("TUN", DirInbound, udpPacket())
		out := buf.String()
		if got := strings.Contains(out, "[TUN] <- 29 bytes - UDP"); got != c.line {
			t.Errorf("level %d: packet line=%v, want %v\n%s", c.level, got, c.line, out)
		}
		if got := strings.Contains(out, "hexdump:"); got != c.hexdump {
			t.Errorf("level %d: hexdump=%v, want %v\n%s", c.level, got, c.hexdump, out)
		}
	}
}
