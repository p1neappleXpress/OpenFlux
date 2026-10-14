//go:build !windows && !linux

package netbind

import "errors"

// BindDefault is only needed (and implemented) for the Windows full tunnel.
func BindDefault() (uint32, error) {
	return 0, errors.New("привязка к интерфейсу есть только в Windows")
}
