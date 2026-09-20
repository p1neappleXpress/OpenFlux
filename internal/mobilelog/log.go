// Package mobilelog bounds and sanitizes the log stream crossing the iOS bridge.
package mobilelog

import (
	"regexp"
	"strings"
	"sync"
)

var urls = regexp.MustCompile(`(?i)(?:https?|wss?)://[^\s"<>]+`)

type Buffer struct {
	mu      sync.Mutex
	lines   []string
	bytes   int
	secrets []string
}

func New() *Buffer { return &Buffer{} }

func (b *Buffer) SetSecrets(values ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.secrets = nil
	for _, v := range values {
		if v != "" {
			b.secrets = append(b.secrets, v)
		}
	}
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(p)
	for _, secret := range b.secrets {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	s = urls.ReplaceAllString(s, "[redacted URL]")
	s = strings.TrimRight(s, "\n")
	if len(s) > 4096 {
		s = s[:4096]
	}
	b.lines = append(b.lines, s)
	b.bytes += len(s)
	for len(b.lines) > 200 || b.bytes > 64<<10 {
		b.bytes -= len(b.lines[0])
		b.lines[0] = ""
		b.lines = b.lines[1:]
	}
	return len(p), nil
}

func (b *Buffer) Drain() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := strings.Join(b.lines, "\n")
	clear(b.lines)
	b.lines = b.lines[:0]
	b.bytes = 0
	return out
}
