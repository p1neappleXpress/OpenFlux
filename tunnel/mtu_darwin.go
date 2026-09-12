//go:build darwin

package tunnel

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func platformInterfaceMTU(name string) (int, error) {
	out, err := exec.Command("ifconfig", name).Output()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "mtu ") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				return strconv.Atoi(f[1])
			}
		}
	}
	return 0, fmt.Errorf("mtu not found for %q", name)
}
