package tunnel

import (
	"strings"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS answers the device's DNS queries itself, with addresses from
// 198.18.0.0/16 (reserved for benchmarking, never routed on the Internet), and
// remembers which name each one stands for. The stream mode carries TCP only,
// so a name is not resolved here: the connection to its fake address is opened
// by NAME at the exit, which resolves it there (no lookup leaves the device, and
// the answer is the one the exit's side of the Internet gives).
type fakeDNS struct {
	mu     sync.Mutex
	byName map[string]uint32
	names  []string // index -> name; a ring: the oldest mapping is reused when it is full
	next   uint32
}

const fakeDNSSize = 65000

func newFakeDNS() *fakeDNS {
	return &fakeDNS{byName: map[string]uint32{}, names: make([]string, fakeDNSSize), next: 1}
}

func normName(n string) string { return strings.ToLower(strings.TrimSuffix(n, ".")) }

// ipFor is the fake address of name, made on first use.
func (d *fakeDNS) ipFor(name string) [4]byte {
	name = normName(name)
	d.mu.Lock()
	defer d.mu.Unlock()
	idx, ok := d.byName[name]
	if !ok {
		idx = d.next
		d.next++
		if d.next >= fakeDNSSize {
			d.next = 1
		}
		if old := d.names[idx]; old != "" {
			delete(d.byName, old)
		}
		d.names[idx] = name
		d.byName[name] = idx
	}
	return [4]byte{198, 18, byte(idx >> 8), byte(idx)}
}

// nameFor is the name a fake address stands for.
func (d *fakeDNS) nameFor(ip [4]byte) (string, bool) {
	if ip[0] != 198 || ip[1] != 18 {
		return "", false
	}
	idx := uint32(ip[2])<<8 | uint32(ip[3])
	if idx == 0 || idx >= fakeDNSSize {
		return "", false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.names[idx]
	return n, n != ""
}

// answer builds the reply to one DNS query datagram, or nil when it is not a
// query we understand. A names gets a fake address; every other record type
// (AAAA, HTTPS, TXT, ...) gets an empty, successful answer, which makes
// an app use the IPv4 address it was given.
func (d *fakeDNS) answer(query []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) == 0 {
		return nil
	}
	b := dnsmessage.NewBuilder(make([]byte, 0, 512), dnsmessage.Header{
		ID: h.ID, Response: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true,
		RCode: dnsmessage.RCodeSuccess,
	})
	b.EnableCompression()
	if b.StartQuestions() != nil {
		return nil
	}
	for _, q := range qs {
		if b.Question(q) != nil {
			return nil
		}
	}
	if b.StartAnswers() != nil {
		return nil
	}
	for _, q := range qs {
		if q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET {
			res := dnsmessage.AResource{A: d.ipFor(q.Name.String())}
			if b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, res) != nil {
				return nil
			}
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}
