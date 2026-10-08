package script

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// echoScript is a minimal, network-free script transport: it "connects"
// immediately, emits one fixed packet, and echoes whatever write() is
// asked to send via raise() so the test can observe it. It exercises the
// full pipeline (signature verify -> goja bootstrap -> event loop ->
// info()/open() -> emit()/write()/raise()) without touching the network.
const echoScript = `
var Transport = {
  info: function () {
    return { name: "echo", version: "1.0.0" };
  },
  open: function (cfg) {
    setState("connected");
    emit(base64.decode("aGVsbG8=")); // bytes for "hello"
  },
  write: function (bytes) {
    raise("wrote", { b64: base64.encode(bytes) });
  },
  close: function () {
    setState("dead");
  },
};
`

func mustSignedScript(t *testing.T, src string) (path string, pub ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	dir := t.TempDir()
	path = filepath.Join(dir, "echo.js")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	sig := ed25519.Sign(priv, []byte(src))
	if err := os.WriteFile(path+".sig", sig, 0o644); err != nil {
		t.Fatalf("write sig: %v", err)
	}
	return path, pub
}

func TestScriptTransportEndToEnd(t *testing.T) {
	path, pub := mustSignedScript(t, echoScript)

	tr, err := New("echo", path, pub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recvCh := make(chan []byte, 1)
	tr.Receive(func(b []byte) { recvCh <- b })

	wroteCh := make(chan map[string]interface{}, 1)
	tr.SetEventHandler(func(kind string, payload map[string]interface{}) {
		if kind == "wrote" {
			wroteCh <- payload
		}
	})

	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()

	select {
	case got := <-recvCh:
		if string(got) != "hello" {
			t.Fatalf("emit(): got %q, want %q", got, "hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emit()")
	}

	if !tr.IsConnected() {
		t.Fatal("expected IsConnected() == true after setState(\"connected\")")
	}
	if got := tr.Info().Name; got != "echo" {
		t.Fatalf("Info().Name = %q, want %q", got, "echo")
	}

	if err := tr.Send([]byte("ping")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case payload := <-wroteCh:
		b64, _ := payload["b64"].(string)
		got, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("decode write() payload: %v", err)
		}
		if string(got) != "ping" {
			t.Fatalf("write(): got %q, want %q", got, "ping")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for write()")
	}
}

// rawEchoScript proves the fast path itself, with no base64 anywhere:
// emit() takes a plain Uint8Array's buffer built in JS, and write() gets a
// genuine ArrayBuffer object (not a base64 string) with the right length.
const rawEchoScript = `
var Transport = {
  info: function () {
    return { name: "raw-echo", version: "1.0.0" };
  },
  open: function (cfg) {
    setState("connected");
    var u8 = new Uint8Array([104, 101, 108, 108, 111]); // "hello", no codec
    emit(u8.buffer);
  },
  write: function (bytes) {
    raise("wrote", {
      isArrayBuffer: bytes instanceof ArrayBuffer,
      len: bytes.byteLength,
    });
  },
  close: function () {
    setState("dead");
  },
};
`

func TestScriptTransportBinaryFastPath(t *testing.T) {
	path, pub := mustSignedScript(t, rawEchoScript)

	tr, err := New("raw-echo", path, pub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recvCh := make(chan []byte, 1)
	tr.Receive(func(b []byte) { recvCh <- b })

	wroteCh := make(chan map[string]interface{}, 1)
	tr.SetEventHandler(func(kind string, payload map[string]interface{}) {
		if kind == "wrote" {
			wroteCh <- payload
		}
	})

	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()

	select {
	case got := <-recvCh:
		if string(got) != "hello" {
			t.Fatalf("emit(ArrayBuffer): got %q, want %q", got, "hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emit()")
	}

	if err := tr.Send([]byte("ping")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case payload := <-wroteCh:
		if isBuf, _ := payload["isArrayBuffer"].(bool); !isBuf {
			t.Fatalf("write() did not receive an ArrayBuffer: %+v", payload)
		}
		var length int64
		switch l := payload["len"].(type) {
		case int64:
			length = l
		case float64:
			length = int64(l)
		default:
			t.Fatalf("write() byteLength has unexpected type %T: %v", payload["len"], payload["len"])
		}
		if length != 4 {
			t.Fatalf("write() byteLength = %v, want 4", length)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for write()")
	}
}

func TestScriptTransportRejectsBadSignature(t *testing.T) {
	path, _ := mustSignedScript(t, echoScript)
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	if _, err := New("echo", path, otherPub, "", nil, transport.DefaultConfig()); err == nil {
		t.Fatal("expected New() to fail against the wrong public key")
	}
}

// concurrencyPoolScript submits 8 tasks to a pool of size 3, each holding
// its slot for 50ms; it tracks the maximum number of tasks that were ever
// concurrently inside the "held" section and reports it once every task
// has finished. If the pool doesn't actually gate concurrency, max will
// exceed 3.
const concurrencyPoolScript = `
var Transport = {
  info: function () { return { name: "pool-test", version: "1.0.0" }; },
  open: function (cfg) {
    setState("connected");
    var pool = concurrency.pool(3);
    var current = 0;
    var max = 0;
    var done = 0;
    var total = 8;
    for (var i = 0; i < total; i++) {
      pool.run(function () {
        return new Promise(function (resolve) {
          current++;
          if (current > max) max = current;
          setTimeout(function () {
            current--;
            done++;
            if (done === total) raise("done", { max: max });
            resolve(true);
          }, 50);
        });
      });
    }
  },
  write: function (bytes) {},
  close: function () {},
};
`

func TestScriptTransportConcurrencyPool(t *testing.T) {
	path, pub := mustSignedScript(t, concurrencyPoolScript)
	tr, err := New("pool-test", path, pub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	doneCh := make(chan map[string]interface{}, 1)
	tr.SetEventHandler(func(kind string, payload map[string]interface{}) {
		if kind == "done" {
			doneCh <- payload
		}
	})

	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()

	select {
	case payload := <-doneCh:
		var max int64
		switch v := payload["max"].(type) {
		case int64:
			max = v
		case float64:
			max = int64(v)
		}
		if max > 3 {
			t.Fatalf("pool(3) let %d tasks run concurrently, want <= 3", max)
		}
		if max < 1 {
			t.Fatalf("pool never ran anything: max=%d", max)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for all pool tasks to finish")
	}
}

// sessionIsolationScript sets a different cookie on two independent
// http.newSession() sessions, then reports both back via raise() so the
// test can confirm they never leaked into each other or the default jar.
const sessionIsolationScript = `
var Transport = {
  info: function () { return { name: "session-test", version: "1.0.0", cookieDomain: "https://example.com/" }; },
  open: function (cfg) {
    setState("connected");
    var a = http.newSession();
    var b = http.newSession();
    var u = "https://example.com/";
    a.cookies.set(u, { tok: "A" });
    b.cookies.set(u, { tok: "B" });
    cookieJar.set({ tok: "default" });
    raise("done", {
      a: a.cookies.get(u).tok,
      b: b.cookies.get(u).tok,
      def: cookieJar.get().tok,
    });
  },
  write: function (bytes) {},
  close: function () {},
};
`

func TestScriptTransportSessionIsolation(t *testing.T) {
	path, pub := mustSignedScript(t, sessionIsolationScript)
	tr, err := New("session-test", path, pub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	doneCh := make(chan map[string]interface{}, 1)
	tr.SetEventHandler(func(kind string, payload map[string]interface{}) {
		if kind == "done" {
			doneCh <- payload
		}
	})

	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()

	select {
	case payload := <-doneCh:
		if payload["a"] != "A" || payload["b"] != "B" || payload["def"] != "default" {
			t.Fatalf("sessions leaked into each other: %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for done")
	}
}

// udpEchoScript dials the address cfg.url is given and echoes exactly what
// it receives back to the same socket, reporting each round trip via
// raise("recv", ...) so the test can assert on it.
const udpEchoScript = `
var Transport = {
  info: function () { return { name: "udp-test", version: "1.0.0" }; },
  open: function (cfg) {
    udp.open(cfg.url, { readTimeoutMs: 3000 }).then(function (sock) {
      sock.onmessage = function (bytes) {
        raise("recv", { b64: base64.encode(bytes) });
      };
      sock.send(base64.decode("aGVsbG8=")); // "hello"
      setState("connected");
    }, function (e) {
      setState("dead", String(e));
    });
  },
  write: function (bytes) {},
  close: function () {},
};
`

// TestScriptTransportFluxPackage proves a .flux package loads and runs
// exactly like a plain signed .js: same echoScript, packed with a manifest
// via WritePackage instead of a detached .sig file.
func TestScriptTransportFluxPackage(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "echo.flux")
	manifest := PackageManifest{Name: "echo", Version: "1.0.0", Author: "test", Description: "flux smoke test"}
	if err := WritePackage(path, manifest, []byte(echoScript), nil, priv, true); err != nil {
		t.Fatalf("WritePackage: %v", err)
	}

	tr, err := New("echo", path, pub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tr.Package() == nil || tr.Package().Manifest.Author != "test" {
		t.Fatalf("Package() = %+v, want manifest.Author = \"test\"", tr.Package())
	}

	recvCh := make(chan []byte, 1)
	tr.Receive(func(b []byte) { recvCh <- b })

	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()

	select {
	case got := <-recvCh:
		if string(got) != "hello" {
			t.Fatalf("emit(): got %q, want %q", got, "hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emit()")
	}
}

// TestScriptTransportFluxRejectsTamperedManifest proves the signature covers
// the WHOLE manifest, not just the script: flipping author after signing
// must invalidate the package, even though main.js itself is untouched.
func TestScriptTransportFluxRejectsTamperedManifest(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "echo.flux")
	manifest := PackageManifest{Name: "echo", Version: "1.0.0", Author: "legit"}
	if err := WritePackage(path, manifest, []byte(echoScript), nil, priv, true); err != nil {
		t.Fatalf("WritePackage: %v", err)
	}

	// Reopen the archive and rewrite manifest.json's author in place,
	// leaving main.js and package.sig exactly as signed.
	if err := tamperZipEntry(path, "manifest.json", func(b []byte) []byte {
		return []byte(strings.Replace(string(b), "legit", "attacker", 1))
	}); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if _, err := LoadSignedPackage(path, pub); err == nil {
		t.Fatal("expected LoadSignedPackage to reject a tampered manifest")
	}
}

func tamperZipEntry(path, name string, mutate func([]byte) []byte) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	entries := map[string][]byte{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			r.Close()
			return err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			r.Close()
			return err
		}
		if f.Name == name {
			b = mutate(b)
		}
		entries[f.Name] = b
	}
	r.Close()

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for n, b := range entries {
		w, err := zw.Create(n)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return zw.Close()
}

// webrtcCallerScript/webrtcCalleeScript prove the webrtc.* host primitive
// does a REAL ICE/DTLS/SCTP handshake and DataChannel exchange, not just
// exercise the API surface: two ScriptTransports, wired together purely
// through the test's own SDP/ICE relay (onEvent/raise - the same OOB event
// mechanism every transport already uses for captcha/cookie exchange), end
// up with a live DataChannel and pass a real message over it. No STUN/TURN
// needed - both peers gather host candidates and connect directly since
// they're on the same machine.
const webrtcCallerScript = `
var pendingCandidates = [];
var haveRemoteDesc = false;
var pc = null;
var Transport = {
  info: function () { return { name: "webrtc-caller", version: "1.0.0" }; },
  open: function (cfg) {
    pc = webrtc.newPeerConnection({});
    pc.onicecandidate = function (c) {
      if (c) raise("ice", c);
    };
    pc.onconnectionstatechange = function (s) {
      if (s === "connected") setState("connected");
    };
    var dc = pc.createDataChannel("test", { ordered: true });
    dc.onopen = function () { raise("dcopen", {}); };
    pc.createOffer().then(function (sdp) {
      return pc.setLocalDescription("offer", sdp).then(function () {
        raise("sdp", { type: "offer", sdp: sdp });
      });
    });
    Transport._dc = dc;
  },
  write: function (bytes) { Transport._dc.send(bytes); },
  close: function () { if (pc) pc.close(); },
  onEvent: function (kind, payload) {
    if (kind === "remoteSDP") {
      pc.setRemoteDescription(payload.type, payload.sdp).then(function () {
        haveRemoteDesc = true;
        pendingCandidates.forEach(function (c) { pc.addIceCandidate(c); });
        pendingCandidates = [];
      });
    } else if (kind === "remoteICE") {
      if (haveRemoteDesc) pc.addIceCandidate(payload); else pendingCandidates.push(payload);
    }
  },
};
`

const webrtcCalleeScript = `
var pendingCandidates = [];
var haveRemoteDesc = false;
var pc = null;
var Transport = {
  info: function () { return { name: "webrtc-callee", version: "1.0.0" }; },
  open: function (cfg) {
    pc = webrtc.newPeerConnection({});
    pc.onicecandidate = function (c) {
      if (c) raise("ice", c);
    };
    pc.ondatachannel = function (dc) {
      dc.onmessage = function (bytes) { emit(bytes); };
      Transport._dc = dc;
      setState("connected");
    };
  },
  write: function (bytes) { if (Transport._dc) Transport._dc.send(bytes); },
  close: function () { if (pc) pc.close(); },
  onEvent: function (kind, payload) {
    if (kind === "remoteSDP") {
      pc.setRemoteDescription(payload.type, payload.sdp).then(function () {
        haveRemoteDesc = true;
        pendingCandidates.forEach(function (c) { pc.addIceCandidate(c); });
        pendingCandidates = [];
        return pc.createAnswer();
      }).then(function (sdp) {
        if (!sdp) return;
        return pc.setLocalDescription("answer", sdp).then(function () {
          raise("sdp", { type: "answer", sdp: sdp });
        });
      });
    } else if (kind === "remoteICE") {
      if (haveRemoteDesc) pc.addIceCandidate(payload); else pendingCandidates.push(payload);
    }
  },
};
`

func TestScriptTransportWebRTCDataChannel(t *testing.T) {
	callerPath, callerPub := mustSignedScript(t, webrtcCallerScript)
	calleePath, calleePub := mustSignedScript(t, webrtcCalleeScript)

	caller, err := New("webrtc-caller", callerPath, callerPub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New(caller): %v", err)
	}
	callee, err := New("webrtc-callee", calleePath, calleePub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New(callee): %v", err)
	}

	// Relay each side's raise("sdp"/"ice", ...) into the other's
	// Deliver("remoteSDP"/"remoteICE", ...) - exactly the signaling a real
	// oneme.js would do over the MAX WebSocket, just done directly in Go.
	caller.SetEventHandler(func(kind string, payload map[string]interface{}) {
		switch kind {
		case "sdp":
			_ = callee.Deliver("remoteSDP", payload)
		case "ice":
			_ = callee.Deliver("remoteICE", payload)
		}
	})
	callee.SetEventHandler(func(kind string, payload map[string]interface{}) {
		switch kind {
		case "sdp":
			_ = caller.Deliver("remoteSDP", payload)
		case "ice":
			_ = caller.Deliver("remoteICE", payload)
		}
	})

	recvCh := make(chan []byte, 1)
	callee.Receive(func(b []byte) { recvCh <- b })

	// callee first: goja drains a resolved promise's .then chain synchronously
	// on return from the native call that resolved it, so the caller's
	// open() - createOffer().then(...) resolves and raises "sdp" before
	// caller.Start() itself returns. The callee must already be started (its
	// loop live) to receive that Deliver call.
	if err := callee.Start(); err != nil {
		t.Fatalf("callee.Start: %v", err)
	}
	defer callee.Stop()
	if err := caller.Start(); err != nil {
		t.Fatalf("caller.Start: %v", err)
	}
	defer caller.Stop()

	deadline := time.After(10 * time.Second)
	for !caller.IsConnected() || !callee.IsConnected() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for connection: caller=%v callee=%v", caller.IsConnected(), callee.IsConnected())
		case <-time.After(50 * time.Millisecond):
		}
	}

	if err := caller.Send([]byte("hello-over-real-datachannel")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case got := <-recvCh:
		if string(got) != "hello-over-real-datachannel" {
			t.Fatalf("got %q, want %q", got, "hello-over-real-datachannel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the DataChannel message")
	}
}

// webrtcMediaCallerScript is webrtcCallerScript with a recvonly audio m-line (what an SFU that expects a
// conference client wants next to the data channel) and one text and one binary message.
const webrtcMediaCallerScript = `
var pendingCandidates = [];
var haveRemoteDesc = false;
var pc = null;
var Transport = {
  info: function () { return { name: "webrtc-media-caller", version: "1.0.0" }; },
  open: function (cfg) {
    pc = webrtc.newPeerConnection({});
    pc.addTransceiver("audio", { direction: "recvonly" });
    pc.onicecandidate = function (c) { if (c) raise("ice", c); };
    pc.onconnectionstatechange = function (s) { if (s === "connected") setState("connected"); };
    var dc = pc.createDataChannel("test", { ordered: true });
    dc.onopen = function () { dc.send("signal-text"); dc.send(new Uint8Array([1, 2, 3]).buffer); };
    pc.createOffer().then(function (sdp) {
      return pc.setLocalDescription("offer", sdp).then(function () { raise("sdp", { type: "offer", sdp: sdp }); });
    });
  },
  write: function (bytes) {},
  close: function () { if (pc) pc.close(); },
  onEvent: function (kind, payload) {
    if (kind === "remoteSDP") {
      pc.setRemoteDescription(payload.type, payload.sdp).then(function () {
        haveRemoteDesc = true;
        pendingCandidates.forEach(function (c) { pc.addIceCandidate(c); });
        pendingCandidates = [];
      });
    } else if (kind === "remoteICE") {
      if (haveRemoteDesc) pc.addIceCandidate(payload); else pendingCandidates.push(payload);
    }
  },
};
`

// webrtcMediaCalleeScript reports every data channel message with the flag the host passes along, so the test sees
// which one arrived as text and which as bytes.
const webrtcMediaCalleeScript = `
var pendingCandidates = [];
var haveRemoteDesc = false;
var pc = null;
var Transport = {
  info: function () { return { name: "webrtc-media-callee", version: "1.0.0" }; },
  open: function (cfg) {
    pc = webrtc.newPeerConnection({});
    pc.onicecandidate = function (c) { if (c) raise("ice", c); };
    pc.ondatachannel = function (dc) {
      setState("connected");
      dc.onmessage = function (bytes, isString) { raise("got", { n: bytes.byteLength, text: isString === true }); };
    };
  },
  write: function (bytes) {},
  close: function () { if (pc) pc.close(); },
  onEvent: function (kind, payload) {
    if (kind === "remoteSDP") {
      pc.setRemoteDescription(payload.type, payload.sdp).then(function () {
        haveRemoteDesc = true;
        pendingCandidates.forEach(function (c) { pc.addIceCandidate(c); });
        pendingCandidates = [];
        return pc.createAnswer();
      }).then(function (sdp) {
        if (!sdp) return;
        return pc.setLocalDescription("answer", sdp).then(function () { raise("sdp", { type: "answer", sdp: sdp }); });
      });
    } else if (kind === "remoteICE") {
      if (haveRemoteDesc) pc.addIceCandidate(payload); else pendingCandidates.push(payload);
    }
  },
};
`

func TestScriptTransportWebRTCTransceiverAndMessageKind(t *testing.T) {
	callerPath, callerPub := mustSignedScript(t, webrtcMediaCallerScript)
	calleePath, calleePub := mustSignedScript(t, webrtcMediaCalleeScript)
	caller, err := New("webrtc-media-caller", callerPath, callerPub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New(caller): %v", err)
	}
	callee, err := New("webrtc-media-callee", calleePath, calleePub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New(callee): %v", err)
	}

	offer := make(chan string, 1)
	got := make(chan map[string]interface{}, 4)
	caller.SetEventHandler(func(kind string, payload map[string]interface{}) {
		switch kind {
		case "sdp":
			select {
			case offer <- payload["sdp"].(string):
			default:
			}
			_ = callee.Deliver("remoteSDP", payload)
		case "ice":
			_ = callee.Deliver("remoteICE", payload)
		}
	})
	callee.SetEventHandler(func(kind string, payload map[string]interface{}) {
		switch kind {
		case "sdp":
			_ = caller.Deliver("remoteSDP", payload)
		case "ice":
			_ = caller.Deliver("remoteICE", payload)
		case "got":
			got <- payload
		}
	})
	if err := callee.Start(); err != nil {
		t.Fatalf("callee.Start: %v", err)
	}
	defer callee.Stop()
	if err := caller.Start(); err != nil {
		t.Fatalf("caller.Start: %v", err)
	}
	defer caller.Stop()

	select {
	case sdp := <-offer:
		if !strings.Contains(sdp, "m=audio") || !strings.Contains(sdp, "a=recvonly") {
			t.Fatalf("the offer has no recvonly audio line:\n%s", sdp)
		}
		if !strings.Contains(sdp, "m=application") {
			t.Fatalf("the offer lost its data channel line:\n%s", sdp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no offer")
	}

	kinds := map[bool]int{}
	deadline := time.After(10 * time.Second)
	for len(kinds) < 2 {
		select {
		case m := <-got:
			kinds[m["text"] == true]++
		case <-deadline:
			t.Fatalf("messages seen by kind (true = text): %v", kinds)
		}
	}
	if kinds[true] != 1 || kinds[false] != 1 {
		t.Fatalf("want one text and one binary message, got %v", kinds)
	}
}

// scopeCookiesScript mirrors yandex.js/vyandex.js's actual manifest shape:
// cookieDomain is ALREADY the apex ("https://yandex.ru/"), not a subdomain -
// scopeCookiesToParentDomain must still end up cross-subdomain-usable.
const scopeCookiesScript = `
var Transport = {
  info: function () {
    return { name: "scope-test", version: "1.0.0", cookieDomain: "https://yandex.ru/", scopeCookiesToParentDomain: true };
  },
  open: function (cfg) { setState("connected"); },
  write: function (bytes) {},
  close: function () {},
};
`

func TestScriptTransportApplyCookiesScopesToApexDomain(t *testing.T) {
	path, pub := mustSignedScript(t, scopeCookiesScript)
	tr, err := New("scope-test", path, pub, "", nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()

	if err := tr.ApplyCookies(map[string]string{"sessid": "abc123"}); err != nil {
		t.Fatalf("ApplyCookies: %v", err)
	}

	// The cookie must be sendable on a request to a SUBDOMAIN
	// (disk.yandex.ru), not just the exact apex host it was applied against -
	// that's the entire point of scopeCookiesToParentDomain. A host-only
	// cookie (empty Domain) would fail this.
	sub, _ := url.Parse("https://disk.yandex.ru/i/whatever")
	got := tr.cookieJar.Cookies(sub)
	found := false
	for _, c := range got {
		if c.Name == "sessid" && c.Value == "abc123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cookie not visible on disk.yandex.ru after ApplyCookies against apex yandex.ru: %+v", got)
	}
}

func TestScriptTransportUDP(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(buf[:n], addr)
		}
	}()

	path, pub := mustSignedScript(t, udpEchoScript)
	tr, err := New("udp-test", path, pub, conn.LocalAddr().String(), nil, transport.DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recvCh := make(chan string, 1)
	tr.SetEventHandler(func(kind string, payload map[string]interface{}) {
		if kind == "recv" {
			b64, _ := payload["b64"].(string)
			recvCh <- b64
		}
	})

	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()

	select {
	case b64 := <-recvCh:
		got, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(got) != "hello" {
			t.Fatalf("udp echo: got %q, want %q", got, "hello")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the UDP echo")
	}
}
