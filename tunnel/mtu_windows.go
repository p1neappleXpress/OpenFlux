//go:build windows

package tunnel

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func platformInterfaceMTU(name string) (int, error) {
	out, err := exec.Command("netsh", "interface", "ipv4", "show", "subinterfaces").Output()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mtu, err := strconv.Atoi(fields[0])
		if err != nil || mtu <= 0 || mtu > 65535 {
			continue
		}
		iface := strings.Join(fields[4:], " ")
		if iface == name {
			return mtu, nil
		}
	}
	return 0, fmt.Errorf("interface %q not found in netsh output", name)
}
