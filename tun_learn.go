//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// HostLearner periodically snapshots TCP connections of the running process
// and records every distinct remote IPv4 it talks to.
type HostLearner struct {
	pid     int
	stop    chan struct{}
	stopped sync.WaitGroup

	mu   sync.Mutex
	seen map[string]map[string]bool // ip -> ports
}

func NewHostLearner() *HostLearner {
	return &HostLearner{
		pid:  os.Getpid(),
		stop: make(chan struct{}),
		seen: make(map[string]map[string]bool),
	}
}

func (l *HostLearner) Start(interval time.Duration) {
	l.stopped.Add(1)
	go func() {
		defer l.stopped.Done()
		// Immediate first snapshot.
		l.snapshot()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-tick.C:
				l.snapshot()
			}
		}
	}()
}

func (l *HostLearner) Stop() {
	close(l.stop)
	l.stopped.Wait()
}

func (l *HostLearner) snapshot() {
	out, err := exec.Command("lsof", "-nP", "-i", "-a", "-p",
		fmt.Sprintf("%d", l.pid)).Output()
	if err != nil {
		// lsof exits with code 1 when there is nothing to report. Not fatal.
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		idx := strings.Index(line, "->")
		if idx < 0 {
			continue
		}
		rest := line[idx+2:]
		if space := strings.IndexAny(rest, " \t"); space > 0 {
			rest = rest[:space]
		}
		colon := strings.LastIndex(rest, ":")
		if colon < 0 {
			continue
		}
		host, port := rest[:colon], rest[colon+1:]
		if !strings.Contains(host, ".") || strings.HasPrefix(host, "127.") {
			continue
		}
		l.mu.Lock()
		if l.seen[host] == nil {
			l.seen[host] = make(map[string]bool)
		}
		l.seen[host][port] = true
		l.mu.Unlock()
	}
}

func (l *HostLearner) Hosts() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.seen))
	for ip := range l.seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func (l *HostLearner) WriteFile(path string) error {
	hosts := l.Hosts()
	var b strings.Builder
	fmt.Fprintf(&b, "# Learned %s by oflx --tun-learn\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "# One IPv4 per line; used as bypass hosts (routes outside the tunnel)\n")
	for _, ip := range hosts {
		b.WriteString(ip)
		b.WriteByte('\n')
	}
	utils.Debugf("[LEARN] writing %d hosts to %s", len(hosts), path)
	return os.WriteFile(path, []byte(b.String()), 0644)
}
