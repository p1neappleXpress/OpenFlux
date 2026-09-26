package l3

import (
	"fmt"
	"sync/atomic"
	"time"

	"openflux/network"
	"openflux/transport"
	"openflux/utils"
)

const clientIP = "10.10.10.2"

var clientIPBytes = [4]byte{10, 10, 10, 2}

type L3Exit struct {
	trans                transport.Transport
	backend              L3Backend
	ct                   *conntrack
	udp                  *udpNAT
	fromClientFragments  reassembler
	fromNetworkFragments reassembler

	// counters
	pktFromTransport atomic.Uint64
	pktToNetwork     atomic.Uint64
	pktFromNetwork   atomic.Uint64
	pktToTransport   atomic.Uint64

	dropBadIPv4      atomic.Uint64
	dropFragmented   atomic.Uint64
	dropNoFlowKey    atomic.Uint64
	dropNoConntrack  atomic.Uint64
	dropNotForUs     atomic.Uint64
	sendToNetErrors  atomic.Uint64
	sendToClientErrs atomic.Uint64
}

func New(trans transport.Transport) (*L3Exit, error) {
	backend, err := newBackend()
	if err != nil {
		return nil, err
	}
	return &L3Exit{
		trans:   trans,
		backend: backend,
		ct:      newConntrack(),
		udp:     newUDPNAT(backend.EgressIP()),
	}, nil
}

func (t *L3Exit) Mode() string { return "l3" }

func (t *L3Exit) Start() error {
	egress := t.backend.EgressIP()
	utils.Debugf("[L3] started: egress=%s client=%s",
		ipStr(ipU32(egress)), clientIP)

	t.trans.Receive(t.handleFromTransport)
	t.backend.Recv(t.handleFromInternet)

	go t.statsLoop()
	return nil
}

func (t *L3Exit) Stop() error {
	t.ct.Close()
	t.udp.Close()
	return t.backend.Close()
}

func (t *L3Exit) handleFromTransport(pkt []byte) {
	t.pktFromTransport.Add(1)

	sl, ok := sliceIPv4(pkt)
	if !ok {
		t.dropBadIPv4.Add(1)
		utils.Debugf("[L3] drop: bad IPv4 slice (%d bytes)", len(pkt))
		return
	}
	pkt = sl

	// Inbound from the client side of the tunnel: log the packet exactly
	// as it arrived, before any SNAT, in the canonical format.
	network.LogPacket("L3", network.DirOutbound, pkt)

	if isFragmentedIPv4(pkt) {
		if ipU32([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]}) != ipU32(clientIPBytes) {
			t.dropNotForUs.Add(1)
			return
		}
		pkt, ok = t.fromClientFragments.add(pkt, time.Now())
		if !ok {
			t.dropFragmented.Add(1)
			return
		}
	}

	k, ok := extractFlowKey(pkt)
	if !ok {
		t.dropNoFlowKey.Add(1)
		utils.Debugf("[L3] drop: no flow key")
		return
	}
	if k.srcIP != ipU32(clientIPBytes) {
		t.dropNotForUs.Add(1)
		return
	}
	original := append([]byte(nil), pkt...)
	if k.proto == 17 {
		if !validUDPChecksums(pkt) {
			t.dropBadIPv4.Add(1)
			return
		}
		if err := t.udp.send(pkt, k, t.sendNetwork); err != nil {
			t.reportSendError(original, err)
			t.sendToNetErrors.Add(1)
			utils.Debugf("[L3] UDP send to network failed: %v", err)
			return
		}
		t.pktToNetwork.Add(1)
		return
	}
	rewriteSNAT(pkt, t.backend.EgressIP())
	fixChecksums(pkt)
	k.srcIP = ipU32(t.backend.EgressIP())
	if !t.ct.Insert(k) {
		t.dropNoConntrack.Add(1)
		return
	}
	if isTCPClosing(pkt) {
		t.ct.Touch(k, true)
	}

	if err := t.sendNetwork(pkt); err != nil {
		t.reportSendError(original, err)
		t.sendToNetErrors.Add(1)
		utils.Debugf("[L3] send to network failed: %v", err)
		return
	}
	t.pktToNetwork.Add(1)
}

func (t *L3Exit) handleFromInternet(pkt []byte) {
	t.pktFromNetwork.Add(1)

	sl, ok := sliceIPv4(pkt)
	if !ok {
		t.dropBadIPv4.Add(1)
		return
	}
	pkt = sl

	egress := t.backend.EgressIP()
	if pkt[16] != egress[0] || pkt[17] != egress[1] ||
		pkt[18] != egress[2] || pkt[19] != egress[3] {
		t.dropNotForUs.Add(1)
		if utils.IsVerbose() {
			utils.Debugf("[L3] %s (not for us)",
				network.FormatPacket(network.DirInbound, pkt))
		}
		return
	}

	// Inbound from the network: log before DNAT.
	network.LogPacket("L3", network.DirInbound, pkt)

	if isFragmentedIPv4(pkt) {
		pkt, ok = t.fromNetworkFragments.add(pkt, time.Now())
		if !ok {
			t.dropFragmented.Add(1)
			return
		}
	}
	if pkt[9] == 1 {
		if !t.translateICMP(pkt) {
			t.dropNoConntrack.Add(1)
			return
		}
		if err := t.trans.Send(pkt); err != nil {
			t.sendToClientErrs.Add(1)
		} else {
			t.pktToTransport.Add(1)
		}
		return
	}
	k, ok := extractFlowKey(pkt)
	if !ok {
		t.dropNoFlowKey.Add(1)
		return
	}
	if k.proto == 17 {
		wire := append([]byte(nil), pkt...)
		if !t.udp.translateReply(pkt, k) {
			t.dropNoConntrack.Add(1)
			return
		}
		if err := t.sendClient(pkt, wire); err != nil {
			t.sendToClientErrs.Add(1)
			return
		}
		t.pktToTransport.Add(1)
		return
	}
	rk := reverseKey(k)
	if !t.ct.Exists(rk) {
		t.dropNoConntrack.Add(1)
		if utils.IsVerbose() && t.dropNoConntrack.Load()%1000 == 1 {
			utils.Debugf("[L3] noct (sampled): %s:%d -> %s:%d proto=%d",
				ipStr(k.srcIP), k.srcPort, ipStr(k.dstIP), k.dstPort, k.proto)
		}
		return
	}
	t.ct.Touch(rk, isTCPClosing(pkt))
	wire := append([]byte(nil), pkt...)
	rewriteDNAT(pkt, clientIPBytes)
	fixChecksums(pkt)

	if err := t.sendClient(pkt, wire); err != nil {
		t.sendToClientErrs.Add(1)
		return
	}
	t.pktToTransport.Add(1)
}

func (t *L3Exit) statsLoop() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	var lastFromTr, lastToNet, lastFromNet, lastToCli uint64

	for {
		select {
		case <-t.ct.stop:
			return
		case <-tick.C:
		}
		fromTr := t.pktFromTransport.Load()
		toNet := t.pktToNetwork.Load()
		fromNet := t.pktFromNetwork.Load()
		toCli := t.pktToTransport.Load()
		dropBad := t.dropBadIPv4.Load()
		dropFragmented := t.dropFragmented.Load()
		dropNoKey := t.dropNoFlowKey.Load()
		dropNoCt := t.dropNoConntrack.Load()
		dropNotUs := t.dropNotForUs.Load()

		utils.Debugf("[L3-STATS] fromTr=%d(+%d) toNet=%d(+%d) | fromNet=%d(+%d) toCli=%d(+%d) | drops: bad=%d fragmented=%d notus=%d nokey=%d noct=%d | errs: toNet=%d toCli=%d",
			fromTr, fromTr-lastFromTr,
			toNet, toNet-lastToNet,
			fromNet, fromNet-lastFromNet,
			toCli, toCli-lastToCli,
			dropBad, dropFragmented, dropNotUs, dropNoKey, dropNoCt,
			t.sendToNetErrors.Load(), t.sendToClientErrs.Load())

		lastFromTr, lastToNet = fromTr, toNet
		lastFromNet, lastToCli = fromNet, toCli
	}
}

func ipU32(b [4]byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func ipStr(ip uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d",
		byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
}

func sliceIPv4(pkt []byte) ([]byte, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return nil, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return nil, false
	}
	tot := int(pkt[2])<<8 | int(pkt[3])
	if tot < ihl || tot > len(pkt) {
		return nil, false
	}
	return pkt[:tot], true
}
