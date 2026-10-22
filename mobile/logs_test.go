package mobile

import (
	"fmt"
	"strings"
	"testing"
)

// The pending log keeps the newest keptLogs lines, and a line costs no
// copy of the whole list: packet logs come hundreds a second, under the
// lock the device's packets take.
func TestAppendLogKeepsTheNewest(t *testing.T) {
	ReadLogs()
	for i := 0; i < 2500; i++ {
		appendLog(fmt.Sprintf("line %d", i))
	}
	lines := strings.Split(ReadLogs(), "\n")
	if len(lines) != keptLogs || lines[0] != fmt.Sprintf("line %d", 2500-keptLogs) || lines[len(lines)-1] != "line 2499" {
		t.Fatalf("%d lines, %q ... %q", len(lines), lines[0], lines[len(lines)-1])
	}
	if ReadLogs() != "" {
		t.Error("lines left after reading")
	}
	// Past keptLogs: the old list copied itself on every line here.
	for i := 0; i < keptLogs+10; i++ {
		appendLog("x")
	}
	allocs := testing.AllocsPerRun(400, func() { appendLog("x") })
	if allocs > 0.5 {
		t.Errorf("%.2f allocations a line: the list is copied on every line", allocs)
	}
	ReadLogs()
}
