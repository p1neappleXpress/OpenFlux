package mobile

import (
	"fmt"
	"sync"

	"openflux/share"
	"openflux/transport"
	"openflux/tunnel"
	"openflux/utils"
)

// Exit mode: the phone runs an l4 exit node, like the CLI's --role=exit
// --mode=l4. Clients reach it through the transport and their traffic
// leaves from the phone's own network through a userspace gVisor stack, so
// no root is needed.
var exitNode = struct {
	mu        sync.Mutex
	running   bool
	transport transport.Transport
	node      tunnel.ExitNode
	// share is what ExitShareLink hands to clients; nil when not running.
	share *share.Config
}{}

// StartExit starts the exit node in classic single-transport mode; the
// arguments are those of Start. Returns "" or a user-readable error.
func StartExit(transportType, documentURL, encryptionSecret, codec, maxToken, maxUid string) string {
	if msg := validateClassic(transportType, documentURL, encryptionSecret); msg != "" {
		return msg
	}
	return startExitWith(func() (transport.Transport, error) {
		return classicTransport(transportType, documentURL, encryptionSecret, codec, maxToken, maxUid, true)
	}, exitShareClassic(transportType, documentURL, encryptionSecret, codec))
}

// StartSessionExit starts the exit node in Session mode (see StartSession).
// direct listens on its address instead of dialing it.
func StartSessionExit(specsJSON, encryptionSecret string) string {
	return startExitWith(func() (transport.Transport, error) {
		return buildSession(specsJSON, encryptionSecret, true)
	}, exitShareSession(specsJSON, encryptionSecret))
}

func startExitWith(build func() (transport.Transport, error), shareCfg *share.Config) string {
	exitNode.mu.Lock()
	if exitNode.running {
		exitNode.mu.Unlock()
		return ""
	}
	exitNode.mu.Unlock()

	utils.SetLevel(int(debugLevel.Load()))
	utils.SetLogSink(appendLog)
	appendLog("[ANDROID] Запуск выходной ноды (l4)")

	fail := func(err error) string {
		appendLog(fmt.Sprintf("[ERROR] Выходная нода: %v", err))
		detachCaptcha()
		clearRoute()
		return err.Error()
	}
	trans, err := build()
	if err != nil {
		return fail(err)
	}
	if err := trans.Start(); err != nil {
		return fail(err)
	}
	node, err := tunnel.NewExitNode(trans, "l4")
	if err == nil {
		err = node.Start()
	}
	if err != nil {
		_ = trans.Stop()
		return fail(err)
	}

	exitNode.mu.Lock()
	exitNode.running, exitNode.transport, exitNode.node, exitNode.share = true, trans, node, shareCfg
	exitNode.mu.Unlock()
	appendLog("[SUCCESS] Выходная нода запущена (l4)")
	return ""
}

func StopExit() {
	exitNode.mu.Lock()
	trans, node := exitNode.transport, exitNode.node
	exitNode.running, exitNode.transport, exitNode.node, exitNode.share = false, nil, nil, nil
	exitNode.mu.Unlock()
	detachCaptcha()
	CancelCaptcha()
	clearRoute()
	appendLog("[ANDROID] Остановка выходной ноды")
	if node != nil {
		_ = node.Stop()
	}
	if trans != nil {
		_ = trans.Stop()
	}
}

func ExitIsRunning() bool {
	exitNode.mu.Lock()
	defer exitNode.mu.Unlock()
	return exitNode.running
}

// ExitIsConnected reports whether a client can use the node: in Session
// mode a client has completed the handshake, in classic mode the carrier
// is up.
func ExitIsConnected() bool {
	t := exitTransport()
	return t != nil && t.IsConnected()
}

// ExitBytesSent and ExitBytesReceived are running carrier totals (to the
// clients and from them), for a live speed indicator.
func ExitBytesSent() int64 {
	if t := exitTransport(); t != nil {
		return int64(t.Stats().BytesSent)
	}
	return 0
}

func ExitBytesReceived() int64 {
	if t := exitTransport(); t != nil {
		return int64(t.Stats().BytesReceived)
	}
	return 0
}

func exitTransport() transport.Transport {
	exitNode.mu.Lock()
	defer exitNode.mu.Unlock()
	return exitNode.transport
}
