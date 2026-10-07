package script

import (
	"github.com/dop251/goja"
	"github.com/pion/webrtc/v4"
)

// registerWebRTC exposes a minimal PeerConnection/DataChannel surface backed
// by pion/webrtc (already a dependency via transport/oneme's native
// implementation - no new module). This is the same class of exception as
// crypto.solvePow: ICE/DTLS/SCTP genuinely cannot be implemented in JS, so
// that layer stays native. Everything ABOVE it - signaling, when to
// createOffer/createAnswer, how candidates get to the other side, retry
// policy - lives entirely in the script, exactly like every other
// transport. No restriction beyond what pion itself exposes: a script can
// dial any ICE server it likes.
func registerWebRTC(vm *goja.Runtime, t *ScriptTransport) {
	webrtcObj := vm.NewObject()
	webrtcObj.Set("newPeerConnection", func(call goja.FunctionCall) goja.Value {
		return newPeerConnectionJS(vm, t, call)
	})
	vm.Set("webrtc", webrtcObj)
}

func newPeerConnectionJS(vm *goja.Runtime, t *ScriptTransport, call goja.FunctionCall) goja.Value {
	config := webrtc.Configuration{}
	if opts, ok := call.Argument(0).(*goja.Object); ok {
		if v := opts.Get("iceServers"); v != nil && !goja.IsUndefined(v) {
			if arr, ok := v.Export().([]interface{}); ok {
				for _, raw := range arr {
					m, ok := raw.(map[string]interface{})
					if !ok {
						continue
					}
					config.ICEServers = append(config.ICEServers, parseICEServer(m))
				}
			}
		}
		if v := opts.Get("iceTransportPolicy"); v != nil && !goja.IsUndefined(v) && v.String() == "relay" {
			config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
		}
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		panic(vm.NewGoError(err))
	}
	t.addCloser(func() { _ = pc.Close() })

	obj := vm.NewObject()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			fn, ok := goja.AssertFunction(obj.Get("onicecandidate"))
			if !ok {
				return
			}
			if c == nil {
				_, _ = fn(obj, goja.Null())
				return
			}
			init := c.ToJSON()
			out := vm.NewObject()
			out.Set("candidate", init.Candidate)
			if init.SDPMid != nil {
				out.Set("sdpMid", *init.SDPMid)
			}
			if init.SDPMLineIndex != nil {
				out.Set("sdpMLineIndex", *init.SDPMLineIndex)
			}
			_, _ = fn(obj, out)
		})
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			if fn, ok := goja.AssertFunction(obj.Get("oniceconnectionstatechange")); ok {
				_, _ = fn(obj, vm.ToValue(s.String()))
			}
		})
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			if fn, ok := goja.AssertFunction(obj.Get("onconnectionstatechange")); ok {
				_, _ = fn(obj, vm.ToValue(s.String()))
			}
		})
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			fn, ok := goja.AssertFunction(obj.Get("ondatachannel"))
			if !ok {
				return
			}
			djs := newDataChannelJS(vm, t, dc)
			_, _ = fn(obj, djs)
		})
	})

	obj.Set("createDataChannel", func(call goja.FunctionCall) goja.Value {
		label := call.Argument(0).String()
		init := &webrtc.DataChannelInit{}
		if o, ok := call.Argument(1).(*goja.Object); ok {
			if v := o.Get("ordered"); v != nil && !goja.IsUndefined(v) {
				ordered := v.ToBoolean()
				init.Ordered = &ordered
			}
			if v := o.Get("maxRetransmits"); v != nil && !goja.IsUndefined(v) {
				mr := uint16(v.ToInteger())
				init.MaxRetransmits = &mr
			}
		}
		dc, err := pc.CreateDataChannel(label, init)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return newDataChannelJS(vm, t, dc)
	})

	// addTransceiver(kind, {direction}) puts an RTP m-line into the next offer. A data-channel-only
	// offer is refused by servers that expect a conference client (MTS-Link's SFU wants an audio
	// line); "recvonly" asks for nothing to be sent, so no media ever flows from here.
	obj.Set("addTransceiver", func(call goja.FunctionCall) goja.Value {
		kind := webrtc.NewRTPCodecType(call.Argument(0).String())
		if kind == 0 {
			panic(vm.NewTypeError(`addTransceiver(kind, init): kind must be "audio" or "video"`))
		}
		init := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}
		if o, ok := call.Argument(1).(*goja.Object); ok {
			if v := o.Get("direction"); v != nil && !goja.IsUndefined(v) {
				d := webrtc.NewRTPTransceiverDirection(v.String())
				if d == 0 {
					panic(vm.NewTypeError(`addTransceiver(kind, init): direction must be "sendrecv", "sendonly", "recvonly" or "inactive"`))
				}
				init.Direction = d
			}
		}
		if _, err := pc.AddTransceiverFromKind(kind, init); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	obj.Set("createOffer", func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := vm.NewPromise()
		desc, err := pc.CreateOffer(nil)
		if err != nil {
			_ = reject(vm.NewGoError(err))
		} else {
			_ = resolve(desc.SDP)
		}
		return vm.ToValue(promise)
	})
	obj.Set("createAnswer", func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := vm.NewPromise()
		desc, err := pc.CreateAnswer(nil)
		if err != nil {
			_ = reject(vm.NewGoError(err))
		} else {
			_ = resolve(desc.SDP)
		}
		return vm.ToValue(promise)
	})
	obj.Set("setLocalDescription", func(call goja.FunctionCall) goja.Value {
		return setDescription(vm, call, pc.SetLocalDescription)
	})
	obj.Set("setRemoteDescription", func(call goja.FunctionCall) goja.Value {
		return setDescription(vm, call, pc.SetRemoteDescription)
	})
	obj.Set("addIceCandidate", func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := vm.NewPromise()
		o, _ := call.Argument(0).(*goja.Object)
		if o == nil {
			_ = reject(vm.NewTypeError("addIceCandidate(init): init must be an object"))
			return vm.ToValue(promise)
		}
		init := webrtc.ICECandidateInit{Candidate: o.Get("candidate").String()}
		if v := o.Get("sdpMid"); v != nil && !goja.IsUndefined(v) {
			s := v.String()
			init.SDPMid = &s
		}
		if v := o.Get("sdpMLineIndex"); v != nil && !goja.IsUndefined(v) {
			idx := uint16(v.ToInteger())
			init.SDPMLineIndex = &idx
		}
		if err := pc.AddICECandidate(init); err != nil {
			_ = reject(vm.NewGoError(err))
		} else {
			_ = resolve(goja.Undefined())
		}
		return vm.ToValue(promise)
	})
	obj.Set("close", func(call goja.FunctionCall) goja.Value {
		_ = pc.Close()
		return goja.Undefined()
	})

	return obj
}

func setDescription(vm *goja.Runtime, call goja.FunctionCall, setFn func(webrtc.SessionDescription) error) goja.Value {
	promise, resolve, reject := vm.NewPromise()
	var sdpType webrtc.SDPType
	switch call.Argument(0).String() {
	case "offer":
		sdpType = webrtc.SDPTypeOffer
	case "answer":
		sdpType = webrtc.SDPTypeAnswer
	default:
		_ = reject(vm.NewTypeError(`setDescription(type, sdp): type must be "offer" or "answer"`))
		return vm.ToValue(promise)
	}
	if err := setFn(webrtc.SessionDescription{Type: sdpType, SDP: call.Argument(1).String()}); err != nil {
		_ = reject(vm.NewGoError(err))
	} else {
		_ = resolve(goja.Undefined())
	}
	return vm.ToValue(promise)
}

func parseICEServer(m map[string]interface{}) webrtc.ICEServer {
	s := webrtc.ICEServer{}
	if urls, ok := m["urls"].([]interface{}); ok {
		for _, u := range urls {
			if str, ok := u.(string); ok {
				s.URLs = append(s.URLs, str)
			}
		}
	} else if u, ok := m["urls"].(string); ok {
		s.URLs = []string{u}
	}
	if v, ok := m["username"].(string); ok {
		s.Username = v
	}
	if v, ok := m["credential"].(string); ok {
		s.Credential = v
		s.CredentialType = webrtc.ICECredentialTypePassword
	}
	return s
}

// newDataChannelJS wraps a *webrtc.DataChannel - one created locally via
// createDataChannel, or one delivered through ondatachannel for a
// remote-initiated channel. send() auto-detects bytes-like vs. string, same
// convention as the ws.open Socket wrapper in host.go.
func newDataChannelJS(vm *goja.Runtime, t *ScriptTransport, dc *webrtc.DataChannel) *goja.Object {
	obj := vm.NewObject()
	obj.Set("label", dc.Label())

	dc.OnOpen(func() {
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			if fn, ok := goja.AssertFunction(obj.Get("onopen")); ok {
				_, _ = fn(obj)
			}
		})
	})
	dc.OnClose(func() {
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			if fn, ok := goja.AssertFunction(obj.Get("onclose")); ok {
				_, _ = fn(obj)
			}
		})
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		data := msg.Data
		isString := msg.IsString
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			fn, ok := goja.AssertFunction(obj.Get("onmessage"))
			if !ok {
				return
			}
			// Always an ArrayBuffer; the second argument says whether the peer sent it as text, for
			// a channel that carries signaling (strings) next to the tunnel's packets (binary).
			_, _ = fn(obj, vm.ToValue(vm.NewArrayBuffer(data)), vm.ToValue(isString))
		})
	})

	obj.Set("send", func(call goja.FunctionCall) goja.Value {
		arg := call.Argument(0)
		if _, isObj := arg.(*goja.Object); isObj {
			if b, err := bytesArg(vm, arg); err == nil {
				if err := dc.Send(b); err != nil {
					panic(vm.NewGoError(err))
				}
				return goja.Undefined()
			}
		}
		if err := dc.SendText(arg.String()); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})
	obj.Set("close", func(call goja.FunctionCall) goja.Value {
		_ = dc.Close()
		return goja.Undefined()
	})

	return obj
}
