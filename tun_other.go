//go:build !darwin && !windows

package main

import (
	"errors"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

var errTUNUnsupported = errors.New("utun client is only supported on macOS")

type TUNClient struct{}

func NewTUNClient(trans transport.Transport, mtu int) (*TUNClient, error) {
	return nil, errTUNUnsupported
}

func (c *TUNClient) Name() string            { return "" }
func (c *TUNClient) Gateway() string         { return "" }
func (c *TUNClient) SetupInterface() error   { return errTUNUnsupported }
func (c *TUNClient) ConfigureDefault() error { return errTUNUnsupported }
func (c *TUNClient) Start()                  {}
func (c *TUNClient) Close() error            { return nil }
func (c *TUNClient) SaveDefault() error      { return errTUNUnsupported }
func (c *TUNClient) RestoreDefault()         {}
func (c *TUNClient) purgeStaleHostRoutes()   {}

type SocketWatcher struct{}

func NewSocketWatcher(gateway string, onStable func()) *SocketWatcher {
	return &SocketWatcher{}
}

func (w *SocketWatcher) Start(interval time.Duration) {}
func (w *SocketWatcher) Stop()                        {}
