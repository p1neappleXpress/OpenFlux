package transport

import "testing"

func TestBatchDecoderResourceLimits(t *testing.T) {
	for name, frame := range map[string][]byte{
		"wire bytes":     append([]byte{batchFormatVersion, 0}, make([]byte, maxFrameBytes+1)...),
		"records":        append([]byte{batchFormatVersion, 0}, make([]byte, (maxFrameRecords+1)*2)...),
		"expanded bytes": append([]byte{batchFormatVersion, batchFlagZstd}, zstdEnc.EncodeAll(make([]byte, maxFrameBytes+1), nil)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeBatch(frame); err == nil {
				t.Fatal("accepted oversized frame")
			}
		})
	}
}

func TestBatchedTransportLifecycle(t *testing.T) {
	b := NewBatchedTransport(&fakeTransport{})
	if b.Send([]byte("before start")) == nil {
		t.Fatal("Send before Start succeeded")
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
	if b.Send([]byte("after stop")) == nil {
		t.Fatal("Send after Stop succeeded")
	}
	if b.Start() == nil {
		t.Fatal("stopped instance restarted")
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultBatchedTransportDoesNotAcceptV3OrNegotiate(t *testing.T) {
	t.Setenv("OPENFLUX_EXPERIMENTAL_WIRE_V3", "")
	f := &fakeTransport{}
	b := NewBatchedTransport(f)
	var received int
	b.Receive(func([]byte) { received++ })
	f.cb(encodeBatch([][]byte{encodeCapabilityRecord(DefaultCapabilities, true)}))
	f.cb(encodeBatchV3([][]byte{[]byte("unexpected")}, 42, 1))
	if _, seen := b.PeerCapabilities(); seen || received != 0 {
		t.Fatal("default mode negotiated or accepted experimental data")
	}
}
