// Package mobile exposes the OpenFlux packet transport to Android through
// gomobile. Android owns the TUN file descriptor; this package only transports
// complete IPv4 packets through the configured Yandex document.
package mobile

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
	"github.com/p1neappleXpress/OpenFlux/transport/oneme"
	"github.com/p1neappleXpress/OpenFlux/transport/yandex"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

var client = packetClient{}

// debugLevel is what Start/StartSession/StartExit/StartProxy set the core's
// logger to on their next connect. Defaults to utils.LevelDebug, the fixed
// level every embedding app got before SetDebugLevel existed.
var debugLevel atomic.Int32

func init() {
	debugLevel.Store(int32(utils.LevelDebug))
}

// SetDebugLevel sets the level Start/StartSession/StartExit/StartProxy put
// the core's logger at on their next connect: 0 off, 1 packet movement
// (-d), 2 operational logs including session/crypto/KDF context (-dd), 3
// packet and frame hexdumps (-ddd). Matches the CLI's --debug=N; call
// before connecting, it only takes effect on the next Start*.
func SetDebugLevel(n int) {
	debugLevel.Store(int32(n))
}

type packetClient struct {
	mu        sync.Mutex
	running   bool
	transport transport.Transport
	packets   [][]byte
	logs      []string
	// arrived is signalled when a packet is queued, for ReadTimeout.
	arrived chan struct{}
}

// lowMemory selects the phone resource profile for the carriers of the
// next start (see SetLowMemory).
var lowMemory atomic.Bool

// SetLowMemory picks the resource profile for the next Start*: on, the
// carriers use small queues and buffers (Volga's slim profile) - what an
// iOS Network Extension (50 MB for the whole process) needs. The wire
// format is the same either way.
func SetLowMemory(on bool) {
	lowMemory.Store(on)
}

func appendLog(message string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.logs = append(client.logs, message)
	if len(client.logs) > 500 {
		client.logs = append([]string(nil), client.logs[len(client.logs)-500:]...)
	}
}

// Start connects the packet transport in classic single-transport mode.
// transportType is "yandex" (default when empty), "vyandex", "boards",
// "mailru", "cupsonline" or "oneme". documentURL is required for all but
// "oneme", which instead needs maxToken (and optionally maxUid). codec is
// "batched" (default, zstd+coalescing, matches the CLI's --codec=batched) or
// "legacy" (per-packet LZ4; both peers must agree). It returns an empty
// string on success and a user-readable error on failure.
func Start(transportType, documentURL, encryptionSecret, codec, maxToken, maxUid string) string {
	if msg := validateClassic(transportType, documentURL, encryptionSecret); msg != "" {
		return msg
	}
	return startPacket(func() (transport.Transport, error) {
		return classicTransport(transportType, documentURL, encryptionSecret, codec, maxToken, maxUid, false)
	})
}

// StartSession connects the packet transport in Session mode (the CLI's
// --negotiate / --transports): several transports at once with failover,
// see buildSession for specsJSON.
func StartSession(specsJSON, encryptionSecret string) string {
	return startPacket(func() (transport.Transport, error) {
		return buildSession(specsJSON, encryptionSecret, false)
	})
}

func startPacket(build func() (transport.Transport, error)) string {
	client.mu.Lock()
	if client.running {
		client.mu.Unlock()
		return ""
	}
	client.running = true
	client.packets = nil
	client.logs = nil
	if client.arrived == nil {
		client.arrived = make(chan struct{}, 1)
	}
	client.mu.Unlock()

	utils.SetLevel(int(debugLevel.Load()))
	utils.SetLogSink(appendLog)

	fail := func(err error) string {
		appendLog(fmt.Sprintf("[ERROR] Ошибка запуска: %v", err))
		client.mu.Lock()
		client.running = false
		client.mu.Unlock()
		detachCaptcha()
		setAuthProxy(nil)
		return err.Error()
	}
	trans, err := build()
	if err != nil {
		return fail(err)
	}
	maxQueue := transport.DefaultConfig().MaxQueueSize
	trans.Receive(func(data []byte) {
		packet := append([]byte(nil), data...)
		client.mu.Lock()
		if !client.running {
			client.mu.Unlock()
			return
		}
		if len(client.packets) >= maxQueue {
			client.packets = client.packets[1:]
		}
		client.packets = append(client.packets, packet)
		arrived := client.arrived
		client.mu.Unlock()
		select {
		case arrived <- struct{}{}:
		default:
		}
	})
	if err := trans.Start(); err != nil {
		return fail(err)
	}

	client.mu.Lock()
	client.transport = trans
	client.mu.Unlock()
	return ""
}

func validateClassic(transportType, documentURL, encryptionSecret string) string {
	if transportType != "oneme" && documentURL == "" {
		return "Ссылка на документ не указана"
	}
	if encryptionSecret != "" && utils.SecretChars(encryptionSecret) < utils.MinSecretChars {
		return fmt.Sprintf("Ключ шифрования должен содержать не менее %d символов", utils.MinSecretChars)
	}
	return ""
}

// classicTransport builds a classic single-transport profile (the CLI's
// --transport=X). exit selects the exit node's side (the phone as the exit).
//
// With a key it is a Session with the classic layering next to it, as the
// CLI does: the client speaks classic until the exit answers the Session
// handshake, then switches (so a classic profile works against every node,
// old and new); a classic exit serves both kinds of client. Without a key
// only classic is possible. Either way the codec is the preferred framing,
// not a requirement: both are accepted and the other one is tried when the
// peer stays silent.
func classicTransport(transportType, documentURL, encryptionSecret, codec, maxToken, maxUid string, exit bool) (transport.Transport, error) {
	if transportType == "" {
		transportType = "yandex"
	}
	appendLog(fmt.Sprintf("[ANDROID] Запуск транспорта %s", transportType))
	params := classicParams(transportType, documentURL, maxToken, maxUid)
	if encryptionSecret != "" {
		specs, err := json.Marshal([]sessionSpec{{
			Name: transportType, Type: transportType, URL: documentURL, Priority: 100, Params: params,
		}})
		if err != nil {
			return nil, err
		}
		t, _, err := buildSessionWith(string(specs), encryptionSecret, exit, sessionOptions{classic: true, codec: codec})
		return t, err
	}

	config := transport.DefaultConfig()
	inner, err := newRawTransport(transportType, documentURL, params, config, exit)
	if err != nil {
		return nil, err
	}
	attachCaptcha(transportType, documentURL, inner)
	setClassicRoute(transportType)
	if exit {
		addExitRoom(transportType, inner)
	}
	// The codec sits under the (absent) encryption layer; see
	// transport.CodecTransport for how the framing is agreed on.
	inner = transport.NewCodecTransport(inner, codec, !exit)
	appendLog("[ANDROID] Шифрование транспорта отключено (ключ не задан): узел с ключом с этим профилем не заговорит")
	return inner, nil
}

// classicParams are the carrier parameters of a classic profile: the MAX
// token and uid, or for direct the address, dialled by a client and
// listened on by an exit.
func classicParams(transportType, documentURL, maxToken, maxUid string) map[string]interface{} {
	params := map[string]interface{}{"token": maxToken, "uid": maxUid}
	if transportType == "direct" {
		params["dial"] = documentURL
		params["listen"] = documentURL
	}
	return params
}

// newRawTransport builds one carrier, like the CLI's transportFactory.
// params: "token"/"uid" for oneme, "dial" (host:port) for direct; on the
// exit side (exit) direct listens on "listen", or on "dial" when only that
// is set, since a profile keeps one address per transport.
func newRawTransport(typ, url string, params map[string]interface{}, config transport.TransportConfig, exit bool) (transport.Transport, error) {
	str := func(key string) string {
		v, _ := params[key].(string)
		return v
	}
	if lowMemory.Load() {
		config.MaxQueueSize = 512
	}
	switch typ {
	case "", "yandex":
		return yandex.NewYandexDocsTransport(url, config), nil
	case "vyandex":
		if lowMemory.Load() {
			return yandex.NewYandexVolgaTransportWithConfig(url, config, yandex.SlimVolgaConfig()), nil
		}
		return yandex.NewYandexVolgaTransport(url, config), nil
	case "boards":
		return yandex.NewBoardsTransport(url, config), nil
	case "mailru":
		return mailru.NewMailruDocsTransport(url, config), nil
	case "cupsonline":
		return cupsonline.NewCupsonlineTransport(url, config, !exit), nil
	case "oneme":
		uid, _ := strconv.ParseInt(str("uid"), 10, 64)
		return oneme.NewOneMeTransport(exit, str("token"), uid, config), nil
	case "direct":
		dcfg := transport.DefaultDirectConfig()
		if exit {
			dcfg.IsExit = true
			if dcfg.ListenAddr = str("listen"); dcfg.ListenAddr == "" {
				dcfg.ListenAddr = str("dial")
			}
			if dcfg.ListenAddr == "" {
				return nil, fmt.Errorf("direct: не указан адрес для прослушивания (host:port)")
			}
			return transport.NewDirectTransport(config, dcfg), nil
		}
		if dcfg.DialAddr = str("dial"); dcfg.DialAddr == "" {
			return nil, fmt.Errorf("direct: не указан адрес ноды (host:port)")
		}
		return transport.NewDirectTransport(config, dcfg), nil
	default:
		return nil, fmt.Errorf("неизвестный тип транспорта %q", typ)
	}
}

func Stop() {
	client.mu.Lock()
	trans := client.transport
	client.running = false
	client.transport = nil
	client.packets = nil
	client.mu.Unlock()
	detachCaptcha()
	CancelCaptcha()
	setAuthProxy(nil)
	clearRoute()
	appendLog("[ANDROID] Остановка транспорта")
	if trans != nil {
		_ = trans.Stop()
	}
}

func IsConnected() bool {
	client.mu.Lock()
	trans := client.transport
	client.mu.Unlock()
	return trans != nil && trans.IsConnected()
}

func Send(packet []byte) string {
	client.mu.Lock()
	trans := client.transport
	running := client.running
	client.mu.Unlock()
	if !running || trans == nil {
		return "Транспорт не запущен"
	}
	if err := trans.Send(packet); err != nil {
		return err.Error()
	}
	return ""
}

// Read returns one received packet, or nil when the queue is empty.
func Read() []byte {
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.packets) == 0 {
		return nil
	}
	packet := client.packets[0]
	client.packets = client.packets[1:]
	return packet
}

// ReadTimeout is Read that waits up to timeoutMs for a packet, for callers
// that block on the tunnel (the iOS packet flow) instead of polling.
func ReadTimeout(timeoutMs int) []byte {
	if p := Read(); p != nil {
		return p
	}
	client.mu.Lock()
	arrived := client.arrived
	client.mu.Unlock()
	if arrived == nil {
		return nil
	}
	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-arrived:
		return Read()
	case <-timer.C:
		return nil
	}
}

// ReadLogs returns and clears the pending log lines.
func ReadLogs() string {
	client.mu.Lock()
	defer client.mu.Unlock()
	logs := strings.Join(client.logs, "\n")
	client.logs = nil
	return logs
}
