package l3

import (
	"encoding/binary"
	"sync"
	"time"
)

const fragmentLifetime = 30 * time.Second
const fragmentByteLimit = 4 << 20
const fragmentFlowLimit = 64
const fragmentPartLimit = 128

type fragmentKey struct {
	src, dst uint32
	id       uint16
	proto    byte
}
type fragmentPart struct {
	offset int
	data   []byte
}
type fragmentSet struct {
	born   time.Time
	header []byte
	parts  []fragmentPart
	end    int
	bytes  int
}
type reassembler struct {
	mu    sync.Mutex
	sets  map[fragmentKey]*fragmentSet
	bytes int
}

// Zero value is usable. Fixed lifetime (not refreshed by incoming fragments),
// per-datagram and global limits, and overlap rejection bound hostile inputs.
func (r *reassembler) add(p []byte, now time.Time) ([]byte, bool) {
	if !isFragmentedIPv4(p) {
		return p, true
	}
	ihl := int(p[0]&15) * 4
	flags := binary.BigEndian.Uint16(p[6:8])
	off := int(flags&0x1fff) * 8
	more := flags&0x2000 != 0
	size := len(p) - ihl
	if flags&0xc000 != 0 || size <= 0 || (more && size%8 != 0) || off+size > 65535-20 || onesComplementSum(p[:ihl]) != 0 {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sets == nil {
		r.sets = make(map[fragmentKey]*fragmentSet)
	}
	for k, s := range r.sets {
		if now.Sub(s.born) >= fragmentLifetime {
			r.remove(k)
		}
	}
	k := fragmentKey{binary.BigEndian.Uint32(p[12:16]), binary.BigEndian.Uint32(p[16:20]), binary.BigEndian.Uint16(p[4:6]), p[9]}
	s := r.sets[k]
	if s == nil {
		if len(r.sets) >= fragmentFlowLimit {
			return nil, false
		}
		s = &fragmentSet{born: now, end: -1}
		r.sets[k] = s
	}
	if len(s.parts) >= fragmentPartLimit || r.bytes+len(p) > fragmentByteLimit {
		r.remove(k)
		return nil, false
	}
	for _, part := range s.parts {
		if off < part.offset+len(part.data) && part.offset < off+size {
			r.remove(k)
			return nil, false
		}
	}
	if s.end >= 0 && (off+size > s.end || (!more && off+size != s.end) || (more && off+size >= s.end)) {
		r.remove(k)
		return nil, false
	}
	if !more {
		for _, part := range s.parts {
			if part.offset+len(part.data) >= off+size {
				r.remove(k)
				return nil, false
			}
		}
		s.end = off + size
	}
	if off == 0 {
		s.header = append([]byte(nil), p[:ihl]...)
	}
	s.parts = append(s.parts, fragmentPart{off, append([]byte(nil), p[ihl:]...)})
	s.bytes += len(p)
	r.bytes += len(p)
	if s.end < 0 || s.header == nil {
		return nil, false
	}
	if s.end+len(s.header) > 65535 {
		r.remove(k)
		return nil, false
	}
	total := 0
	for _, part := range s.parts {
		total += len(part.data)
	}
	if total != s.end {
		return nil, false
	}
	out := make([]byte, len(s.header)+s.end)
	copy(out, s.header)
	for _, part := range s.parts {
		copy(out[len(s.header)+part.offset:], part.data)
	}
	out[6], out[7] = 0, 0
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	fixIPChecksum(out)
	r.remove(k)
	return out, true
}

func (r *reassembler) remove(k fragmentKey) {
	if s := r.sets[k]; s != nil {
		r.bytes -= s.bytes
		delete(r.sets, k)
	}
}
