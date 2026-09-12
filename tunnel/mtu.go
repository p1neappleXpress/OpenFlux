package tunnel

import (
	"fmt"
	"net"

	"universal-bypass-tool/utils"
)

var (
	ifaceName   string
	ifaceMTU    int
	ifaceMTUSet bool

	mtuOverride    int
	mtuOverrideSet bool
)

// SetInterface выбирает сетевой интерфейс для exit-node (по имени).
// MTU туннеля будет автоматически подстроен под MTU этого интерфейса.
// Пустая строка или "auto" — автоопределение по маршруту к 8.8.8.8.
func SetInterface(name string) error {
	if name == "" || name == "auto" {
		ifaceName = ""
		ifaceMTUSet = false
		utils.Debugf("[TUNNEL] iface: auto")
		return nil
	}
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %q: %w", name, err)
	}
	mtu, err := platformInterfaceMTU(ifi.Name)
	if err != nil {
		return fmt.Errorf("interface %q MTU: %w", name, err)
	}
	if mtu <= 0 || mtu > 65535 {
		return fmt.Errorf("interface %q: bogus MTU %d", name, mtu)
	}
	ifaceName = name
	ifaceMTU = mtu
	ifaceMTUSet = true
	utils.Debugf("[TUNNEL] interface %q selected, MTU=%d", name, mtu)
	return nil
}

// SetMTUOverride принудительно задаёт MTU интернет-интерфейса. Перебивает
// автоопределение и --iface. 0 или отрицательное — сбросить override.
func SetMTUOverride(mtu int) {
	if mtu <= 0 {
		mtuOverride = 0
		mtuOverrideSet = false
		utils.Debugf("[TUNNEL] mtu override: cleared")
		return
	}
	if mtu < 576 || mtu > 65535 {
		utils.Debugf("[TUNNEL] mtu override %d out of range, ignored", mtu)
		return
	}
	mtuOverride = mtu
	mtuOverrideSet = true
	utils.Debugf("[TUNNEL] mtu override: %d", mtu)
}

func InterfaceName() string { return ifaceName }

// IfaceMTU — реальный MTU интернет-интерфейса (без вычетов).
// Приоритет: --mtu > --iface > автоопределение > 1500.
func IfaceMTU() uint32 {
	if mtuOverrideSet {
		return uint32(mtuOverride)
	}
	if ifaceMTUSet {
		return uint32(ifaceMTU)
	}
	if auto, err := autoDetectMTU(); err == nil && auto > 0 {
		return uint32(auto)
	}
	return 1500
}

// TunnelMTU — MTU, который надо поставить на gVisor-туннель, чтобы
// клиентские сегменты пролезали в интернет-интерфейс без фрагментации.
//
// overhead = 20 (IP) + 20 (TCP) + 12 (timestamps) + 4 (SACK) + 4 (запас) = 60
func TunnelMTU() uint32 {
	return IfaceMTU()
}

// LocalIPForTunnel возвращает IPv4-адрес выбранного интерфейса (для SNAT).
// Пусто, если интерфейс не выбран или выбран через --mtu.
func LocalIPForTunnel() string {
	if ifaceName == "" {
		return ""
	}
	ifi, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return ""
	}
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

func autoDetectMTU() (int, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr)
	ifis, _ := net.Interfaces()
	for _, ifi := range ifis {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.Equal(local.IP) {
				return platformInterfaceMTU(ifi.Name)
			}
		}
	}
	return 0, fmt.Errorf("no iface for %s", local.IP)
}
