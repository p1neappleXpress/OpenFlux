package fluxcore

import "time"

// fakeClock lets tests drive time deterministically.
type fakeClock struct{ t time.Time }

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }
func (c *fakeClock) now() time.Time    { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// loopLink records everything Send is given.
type loopLink struct{ sent [][]byte }

func (l *loopLink) Send(b []byte) error {
	l.sent = append(l.sent, append([]byte(nil), b...))
	return nil
}
func (l *loopLink) last() []byte {
	if len(l.sent) == 0 {
		return nil
	}
	return l.sent[len(l.sent)-1]
}

// errLink always fails, to exercise the loss path.
type errLink struct{ err error }

func (e *errLink) Send([]byte) error { return e.err }
