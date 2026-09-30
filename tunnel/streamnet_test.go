package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// deviceLink lets a client-side TCPTunnel stand in for the phone or the TUN
// interface: what it sends goes into the StreamNet, what StreamNet sends comes back.
type deviceLink struct {
	n    *StreamNet
	once sync.Once
}

func (d *deviceLink) Start() error                    { return nil }
func (d *deviceLink) Stop() error                     { return nil }
func (d *deviceLink) Send(b []byte) error             { return d.n.Send(b) }
func (d *deviceLink) Receive(cb func([]byte))         { d.n.Receive(cb) }
func (d *deviceLink) IsConnected() bool               { return true }
func (d *deviceLink) Stats() transport.TransportStats { return transport.TransportStats{} }

type dialed struct {
	host string
	port int
}

// fakeExit records what StreamNet asks the exit for and serves an echo on each stream.
type fakeExit struct {
	mu   sync.Mutex
	seen []dialed
	fail bool
}

func (f *fakeExit) dial(_ context.Context, host string, port int) (net.Conn, error) {
	f.mu.Lock()
	f.seen = append(f.seen, dialed{host, port})
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return nil, errors.New("refused")
	}
	a, b := net.Pipe()
	go func() { defer b.Close(); io.Copy(b, b) }()
	return a, nil
}

func (f *fakeExit) last() dialed {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return dialed{}
	}
	return f.seen[len(f.seen)-1]
}

func newRig(t *testing.T) (*TCPTunnel, *StreamNet, *fakeExit) {
	ex := &fakeExit{}
	sn := NewStreamNetDialer(ex.dial)
	if err := sn.Start(); err != nil {
		t.Fatal(err)
	}
	dev := NewTCPTunnelMode(&deviceLink{n: sn}, false, ExitModeL4)
	t.Cleanup(func() { dev.Close(); sn.Stop() })
	return dev, sn, ex
}

func echoOnce(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo: got %q err %v, want %q", buf, err, msg)
	}
}

// A connection to a literal address comes out of the exit as the same address.
func TestStreamNetDialsTheAddressTheDeviceAskedFor(t *testing.T) {
	dev, _, ex := newRig(t)
	c, err := dev.DialTCP("203.0.113.9:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	echoOnce(t, c, "hello through the packet path")
	if d := ex.last(); d.host != "203.0.113.9" || d.port != 443 {
		t.Errorf("the exit was asked for %+v", d)
	}
	// And a lot more than one packet each way, to exercise the stack's windows.
	big := strings.Repeat("0123456789abcdef", 8192) // 128 KB
	done := make(chan error, 1)
	go func() { _, err := c.Write([]byte(big)); done <- err }()
	got := make([]byte, len(big))
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != big {
		t.Fatalf("128 KB round trip: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func dnsQuery(name string, typ dnsmessage.Type) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	out, _ := b.Finish()
	return out
}

func ask(t *testing.T, dev *TCPTunnel, q []byte) *dnsmessage.Message {
	t.Helper()
	c, err := dev.DialUDP("8.8.8.8:53")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no DNS answer: %v", err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	return &m
}

// DNS is answered on the device: a fake address, and connecting to it opens the NAME at the exit.
func TestStreamNetFakeDNSOpensTheNameAtTheExit(t *testing.T) {
	dev, _, ex := newRig(t)
	m := ask(t, dev, dnsQuery("Example.ORG.", dnsmessage.TypeA))
	if len(m.Answers) != 1 {
		t.Fatalf("answers: %+v", m.Answers)
	}
	a := m.Answers[0].Body.(*dnsmessage.AResource).A
	if a[0] != 198 || a[1] != 18 {
		t.Fatalf("not a fake address: %v", a)
	}
	c, err := dev.DialTCP(net.IP(a[:]).String() + ":443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	echoOnce(t, c, "by name")
	if d := ex.last(); d.host != "example.org" || d.port != 443 {
		t.Errorf("the exit was asked for %+v, want example.org:443", d)
	}
	// The same name keeps its address.
	m2 := ask(t, dev, dnsQuery("example.org.", dnsmessage.TypeA))
	if b := m2.Answers[0].Body.(*dnsmessage.AResource).A; b != a {
		t.Errorf("example.org moved from %v to %v", a, b)
	}
}

func TestStreamNetAAAAGetsAnEmptyAnswer(t *testing.T) {
	dev, _, _ := newRig(t)
	m := ask(t, dev, dnsQuery("example.org.", dnsmessage.TypeAAAA))
	if len(m.Answers) != 0 || m.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("AAAA: rcode %v answers %+v, want success and none (so the app uses IPv4)", m.RCode, m.Answers)
	}
}

// When the exit cannot open the stream the app hears a refusal, not a hang.
func TestStreamNetRefusesWhenTheExitFails(t *testing.T) {
	dev, _, ex := newRig(t)
	ex.fail = true
	start := time.Now()
	c, err := dev.DialTCP("203.0.113.9:443")
	if err == nil {
		c.Close()
		t.Fatal("the connection opened although the exit refused")
	}
	if time.Since(start) > 8*time.Second {
		t.Errorf("the refusal took %v", time.Since(start))
	}
}

func TestFakeDNSMapping(t *testing.T) {
	d := newFakeDNS()
	a := d.ipFor("a.example")
	if name, ok := d.nameFor(a); !ok || name != "a.example" {
		t.Errorf("nameFor(%v) = %q %v", a, name, ok)
	}
	if d.ipFor("A.Example.") != a {
		t.Error("case and trailing dot changed the address")
	}
	if _, ok := d.nameFor([4]byte{8, 8, 8, 8}); ok {
		t.Error("a real address taken for a fake one")
	}
	if _, ok := d.nameFor([4]byte{198, 18, 0, 0}); ok {
		t.Error("the zero slot is not a name")
	}
	if d.answer([]byte("not dns")) != nil {
		t.Error("garbage answered")
	}
	// A full ring reuses the oldest slot and forgets its name rather than misdirecting.
	first := d.ipFor("first.example")
	for i := 0; i < fakeDNSSize; i++ {
		d.ipFor("n" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)))
	}
	if name, ok := d.nameFor(first); ok && name == "first.example" {
		t.Error("the oldest name survived a full ring")
	}
}
