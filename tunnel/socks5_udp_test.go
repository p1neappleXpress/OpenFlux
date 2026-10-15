package tunnel

import (
	"testing"
	"time"
)

func TestSOCKS5UDPUnit(t *testing.T) {
	conn, err := dialSOCKS5UDP("127.0.0.1:40001", "1.1.1.1:53")
	if err != nil {
		t.Skipf("No local SOCKS5 proxy on 40001: %v", err)
		return
	}
	defer conn.Close()

	// DNS query for cloudflare.com
	dnsQuery := []byte{
		0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x0a, 'c', 'l', 'o', 'u', 'd', 'f', 'l', 'a', 'r', 'e', 0x03, 'c', 'o', 'm', 0x00,
		0x00, 0x01, 0x00, 0x01,
	}

	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(dnsQuery); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n < 12 || resp[0] != 0x12 || resp[1] != 0x34 {
		t.Fatalf("Unexpected DNS response: %x", resp[:n])
	}
}
