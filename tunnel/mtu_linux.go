//go:build linux

package tunnel

import (
	"os"
	"strconv"
	"strings"
)

func platformInterfaceMTU(name string) (int, error) {
	data, err := os.ReadFile("/sys/class/net/" + name + "/mtu")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
