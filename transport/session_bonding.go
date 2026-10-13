package transport

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Bonding: one Session over all its live carriers at once, so their speeds
// add up and a carrier that drops leaves the others carrying on.
//
// A client asks for it with SubtypeBonding once the handshake is done; an
// exit that bonds answers the same, and both then place packets with the
// bond scheduler and acknowledge what arrived every bondTick
// (SubtypeBondingAck), which is how each side learns its carriers' speed.
// Exits that predate bonding ignore the offer and the client keeps routing
// by flow. The agreement holds for one peer: a new client, or the exit
// restarting, starts without it until offered and answered again.
//
// Documents deliver in bursts, hundreds of milliseconds apart from each
// other, so a TCP connection split packet by packet over several of them
// sees constant reordering, takes it for loss and slows to a crawl (a live
// test: 0.09 MB/s bonded against 0.59 on one document). TCP connections
// therefore stay on one carrier each, new ones going where there is room,
// and move only when their carrier is lost or they have been idle a while;
// many connections add up. UDP (SRT, QUIC) is split packet by packet: those
// protocols put packets back in order in their own buffers, which is what
// lets one SRT stream use every carrier at once.

const (
	bondTick       = 50 * time.Millisecond
	bondOfferEvery = time.Second
	bondVersion    = 1
	// bondFlowlet: a TCP connection idle this long may move to another
	// carrier without its packets overtaking each other.
	bondFlowlet = 500 * time.Millisecond
	// bondFlowForget: a connection idle this long is dropped from the table.
	bondFlowForget = time.Minute
)

// bondState is the Session's bonding side; guarded by Session.mu except
// for sched, which locks itself.
type bondState struct {
	want      bool     // client: offer bonding; exit: accept it
	agreed    bool     // with peer
	peer      [32]byte // the peer it was agreed with
	lastOffer time.Time
	sched     *bondScheduler
	flows     map[flowKeyBytes]*bondFlow
	lastPrune time.Time
	ack       control.BondingAck
	ackDirty  bool
	// ackLink: the carrier data last arrived on. Acks go back on it: it
	// works, where the first carrier by priority may be the one that died,
	// and acks lost there made the working carriers look dead.
	ackLink *transportLink
}

// bondFlow is the carrier a TCP connection rides.
type bondFlow struct {
	link string
	last time.Time
}

// SetBonding makes a client offer bonding to its exit, or an exit refuse
// it (exits accept by default). Call before Start.
func (s *Session) SetBonding(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bond.want = on
}

// Bonding reports whether bonding is on with the current peer.
func (s *Session) Bonding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bondingLocked()
}

// BondStats is what bonding has learned of each carrier, nil when off.
func (s *Session) BondStats() map[string]BondLinkStats {
	s.mu.Lock()
	on, sched := s.bondingLocked(), s.bond.sched
	s.mu.Unlock()
	if !on || sched == nil {
		return nil
	}
	return sched.linkStats()
}

func (s *Session) bondingLocked() bool {
	return s.bond.agreed && s.ready && s.bond.peer == s.peer
}

// agreeBondingLocked switches bonding on for the current peer with fresh
// state: sequence numbers restart with every peer.
func (s *Session) agreeBondingLocked() {
	s.bond.agreed = true
	s.bond.peer = s.peer
	s.bond.sched = newBondScheduler()
	s.bond.flows = make(map[flowKeyBytes]*bondFlow)
	s.bond.ack = control.BondingAck{}
	s.bond.ackDirty = false
}

// receiveBondingLocked handles the bonding control messages. It reports
// false for a message this Session does not take (bonding not wanted),
// which then goes on to the control handler as any unknown subtype would.
// Called with s.mu held; returns with it released when it reports true.
func (s *Session) receiveBondingLocked(link *transportLink, sub control.Subtype, payload []byte) bool {
	switch sub {
	case control.SubtypeBonding:
		if !s.bond.want {
			return false
		}
		was := s.bondingLocked()
		if !was {
			s.agreeBondingLocked()
		}
		exit := s.exit
		n := len(s.links)
		s.mu.Unlock()
		if !was {
			utils.Infof("[SESSION] bonding on: %d carriers at once (TCP connections spread over them, UDP split)", n)
		}
		if exit {
			go func() { _ = s.sendControlVia(link, control.SubtypeBonding, []byte{bondVersion}) }()
		}
		return true
	case control.SubtypeBondingAck:
		on, sched := s.bondingLocked(), s.bond.sched
		s.mu.Unlock()
		if !on {
			return true
		}
		a, err := control.DecodeBondingAck(payload)
		if err != nil {
			utils.Debugf("[SESSION] bonding ack from %q: %v", link.name, err)
			return true
		}
		sched.acked(a, time.Now())
		return true
	}
	return false
}

// bondPickLocked chooses the carrier for data packet p (sequence seq) when
// bonding is on; nil when it is off. Called with s.mu held.
func (s *Session) bondPickLocked(links []*transportLink, seq uint64, p []byte, size int) *transportLink {
	if !s.bondingLocked() || len(links) == 0 {
		return nil
	}
	names := make([]string, len(links))
	for i, l := range links {
		names[i] = l.name
	}
	now := time.Now()
	sched := s.bond.sched
	var name string
	if len(p) > 9 && p[9] == 6 {
		key := extractFlowKeyBytes(p)
		f := s.bond.flows[key]
		if f == nil || now.Sub(f.last) >= bondFlowlet || sched.isLost(f.link) || !contains(names, f.link) {
			// A lost carrier gets probes, never a whole connection.
			live := make([]string, 0, len(names))
			for _, n := range names {
				if !sched.isLost(n) {
					live = append(live, n)
				}
			}
			if len(live) == 0 {
				live = names
			}
			f = &bondFlow{link: sched.pick(live, size, now)}
			s.bond.flows[key] = f
		}
		f.last = now
		name = f.link
	} else {
		name = sched.pick(names, size, now)
	}
	sched.sent(seq, name, size, now)
	for _, l := range links {
		if l.name == name {
			return l
		}
	}
	return links[0]
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// bondAckLocked records data packet seq, arrived on link, for the next ack.
func (s *Session) bondAckLocked(link *transportLink, seq uint64) {
	if s.bondingLocked() {
		s.bond.ack.Set(seq)
		s.bond.ackDirty = true
		s.bond.ackLink = link
	}
}

// bondLoop offers bonding until answered and, once on, acknowledges data
// and gives up on packets lost in flight.
func (s *Session) bondLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(bondTick)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		now := time.Now()
		s.mu.Lock()
		offer := !s.exit && s.bond.want && s.ready && !s.bondingLocked() && now.Sub(s.bond.lastOffer) >= bondOfferEvery
		if offer {
			s.bond.lastOffer = now
		}
		on := s.bondingLocked()
		var ack []byte
		ackLink := s.bond.ackLink
		if on && s.bond.ackDirty {
			ack = s.bond.ack.Encode()
			s.bond.ackDirty = false
		}
		if on && now.Sub(s.bond.lastPrune) >= 10*time.Second {
			s.bond.lastPrune = now
			for k, f := range s.bond.flows {
				if now.Sub(f.last) >= bondFlowForget {
					delete(s.bond.flows, k)
				}
			}
		}
		sched := s.bond.sched
		s.mu.Unlock()

		if offer {
			if err := s.SendControl(control.SubtypeBonding, []byte{bondVersion}); err != nil {
				utils.Debugf("[SESSION] bonding offer: %v", err)
			}
		}
		if !on {
			continue
		}
		sched.expire(now)
		if ack != nil {
			if err := s.sendControlVia(ackLink, control.SubtypeBondingAck, ack); err != nil {
				utils.Debugf("[SESSION] bonding ack: %v", err)
			}
		}
		if utils.Throttled("bond.stats", 10*time.Second) {
			var parts []string
			for name, st := range sched.linkStats() {
				parts = append(parts, fmt.Sprintf("%s %.0f KB/s, %d KB in flight, rtt %v",
					name, st.RateBps/1024, st.InflightBytes/1024, st.RTT.Round(time.Millisecond)))
			}
			sort.Strings(parts)
			utils.Infof("[BOND] %s", strings.Join(parts, "; "))
		}
	}
}
