package transport

// ErrorNotifier is implemented by transports that want to signal
// out-of-band conditions (captcha, login required) to the layer above.
//
// SetErrorNotifier is called once by the manager before Start. The callback
// must not block; it runs on the transport's reconnect goroutine.
//
// transportName and url identify which transport reported the error, so the
// manager can route it to the right IPC channel.
type ErrorNotifier interface {
	SetErrorNotifier(func(err error, transportName, url, reason string))
}
