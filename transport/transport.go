package transport

import (
	"sync"
	"sync/atomic"
	"time"
)

type TransportConfig struct {
	MaxReconnectAttempts int
	ReconnectDelay       time.Duration
	ReconnectMultiplier  float64
	MaxQueueSize         int
	KeepAliveInterval    time.Duration
}

type Transport interface {
	Start() error
	Stop() error
	Send(data []byte) error
	Receive(callback func([]byte))
	IsConnected() bool
	Stats() TransportStats
}

type TransportStats struct {
	BytesSent     uint64
	BytesReceived uint64
	PacketsSent   uint64
	PacketsRecv   uint64
	Reconnects    uint64
	Connected     bool
	Uptime        time.Duration
}

func DefaultConfig() TransportConfig {
	return TransportConfig{
		MaxReconnectAttempts: 999999,
		ReconnectDelay:       0,
		ReconnectMultiplier:  1.1,
		MaxQueueSize:         1024,
		KeepAliveInterval:    10 * time.Second,
	}
}

type BaseTransport struct {
	config    TransportConfig
	running   atomic.Int32
	connected atomic.Int32
	stats     TransportStats
	startTime time.Time

	receiveCallback func([]byte)
	Mu              sync.RWMutex

	reconnectAttempts atomic.Int32

	// done закрывается в Stop(), чтобы ожидания в подклассах (бэкоффы,
	// пауза на капче) прерывались сразу, а не досыпали свой интервал.
	// Отдельный мьютекс, а не Mu: подклассы держат Mu вокруг своей сессии и
	// зовут Stop() из-под него — общий мьютекс дал бы взаимную блокировку.
	doneMu sync.Mutex
	done   chan struct{}
}

func NewBaseTransport(config TransportConfig) *BaseTransport {
	return &BaseTransport{
		config:    config,
		startTime: time.Now(),
		done:      make(chan struct{}),
	}
}

func (b *BaseTransport) Start() error {
	b.doneMu.Lock()
	b.done = make(chan struct{})
	b.doneMu.Unlock()
	b.running.Store(1)
	b.startTime = time.Now()
	return nil
}

func (b *BaseTransport) Stop() error {
	b.running.Store(0)
	b.connected.Store(0)
	b.doneMu.Lock()
	if b.done != nil {
		select {
		case <-b.done: // уже закрыт
		default:
			close(b.done)
		}
	}
	b.doneMu.Unlock()
	return nil
}

// Done возвращает канал, который закрывается при Stop(). Подклассы селектятся
// на нём, чтобы прервать сон.
func (b *BaseTransport) Done() <-chan struct{} {
	b.doneMu.Lock()
	d := b.done
	b.doneMu.Unlock()
	if d == nil {
		return make(chan struct{}) // не стартовали — блокируется навсегда
	}
	return d
}

func (b *BaseTransport) IsRunning() bool {
	return b.running.Load() == 1
}

func (b *BaseTransport) IsConnected() bool {
	return b.connected.Load() == 1
}

func (b *BaseTransport) SetConnected(connected bool) {
	if connected {
		b.connected.Store(1)
	} else {
		b.connected.Store(0)
	}
}

func (b *BaseTransport) Receive(callback func([]byte)) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	b.receiveCallback = callback
}

func (b *BaseTransport) CallReceive(data []byte) {
	b.Mu.RLock()
	cb := b.receiveCallback
	b.Mu.RUnlock()
	if cb != nil {
		cb(data)
	}
}

func (b *BaseTransport) GetSession(accessor func(interface{})) {
	b.Mu.RLock()
	defer b.Mu.RUnlock()
	// This is a helper for subclasses
}

func (b *BaseTransport) Stats() TransportStats {
	return TransportStats{
		BytesSent:     atomic.LoadUint64(&b.stats.BytesSent),
		BytesReceived: atomic.LoadUint64(&b.stats.BytesReceived),
		PacketsSent:   atomic.LoadUint64(&b.stats.PacketsSent),
		PacketsRecv:   atomic.LoadUint64(&b.stats.PacketsRecv),
		Reconnects:    uint64(b.reconnectAttempts.Load()),
		Connected:     b.IsConnected(),
		Uptime:        time.Since(b.startTime),
	}
}

func (b *BaseTransport) RecordSend(bytes int) {
	atomic.AddUint64(&b.stats.BytesSent, uint64(bytes))
	atomic.AddUint64(&b.stats.PacketsSent, 1)
}

func (b *BaseTransport) RecordReceive(bytes int) {
	atomic.AddUint64(&b.stats.BytesReceived, uint64(bytes))
	atomic.AddUint64(&b.stats.PacketsRecv, 1)
}

func (b *BaseTransport) RecordReconnect() {
	b.reconnectAttempts.Add(1)
}

func (b *BaseTransport) GetConfig() TransportConfig {
	return b.config
}
