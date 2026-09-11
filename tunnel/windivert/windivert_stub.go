//go:build !windows

package windivert

import "errors"

var errUnsupported = errors.New("windivert: only supported on windows")

type Endpoint struct{}

func New() (*Endpoint, error)                { return nil, errUnsupported }
func (e *Endpoint) SetOnPacket(func([]byte)) {}
func (e *Endpoint) Send([]byte) error        { return errUnsupported }
func (e *Endpoint) Close()                   {}
func (e *Endpoint) IP() [4]byte              { return [4]byte{} }
func (e *Endpoint) MAC() [6]byte             { return [6]byte{} }
