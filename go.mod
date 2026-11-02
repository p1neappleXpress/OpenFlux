module github.com/p1neappleXpress/OpenFlux

go 1.26.6

require github.com/gorilla/websocket v1.5.3

require gvisor.dev/gvisor v0.0.0-20260530041128-23ef90c42be7

require (
	github.com/google/btree v1.1.2 // indirect
	golang.org/x/exp v0.0.0-20250711185948-6ae5c78190dc // indirect
	golang.org/x/sys v0.48.0
	golang.org/x/time v0.15.0 // indirect
)

require (
	github.com/dop251/goja v0.0.0-20260926152631-39ec2650adc9
	github.com/dop251/goja_nodejs v0.0.0-20260918173711-b481721df8a2
	github.com/google/uuid v1.6.0
	github.com/jlaffaye/ftp v0.2.4
	github.com/klauspost/compress v1.20.0
	github.com/pierrec/lz4/v4 v4.1.27
	github.com/pion/interceptor v0.1.49
	github.com/pion/rtcp v1.2.18
	github.com/pion/rtp v1.10.5
	github.com/pion/sdp/v3 v3.0.20
	github.com/pion/webrtc/v4 v4.2.22
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/xjasonlyu/windivert-go v0.0.0-20201010013527-4239d0afa76f
	golang.org/x/crypto v0.56.0
	golang.org/x/net v0.57.0
	golang.zx2c4.com/wireguard v0.0.0-20260522210424-ecfc5a8d5446
)

require (
	github.com/pion/dtls/v2 v2.2.12 // indirect
	github.com/pion/ice/v2 v2.3.38 // indirect
	github.com/pion/mdns v0.0.12 // indirect
	github.com/pion/srtp/v2 v2.0.20 // indirect
	github.com/pion/stun v0.6.1 // indirect
	github.com/pion/transport/v2 v2.2.10 // indirect
	github.com/pion/turn/v2 v2.1.6 // indirect
	github.com/pion/webrtc/v3 v3.3.6 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)

require (
	github.com/dlclark/regexp2/v2 v2.5.2 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/google/pprof v0.0.0-20240727154555-813a5fbdbec8 // indirect
	github.com/pion/datachannel v1.6.3 // indirect
	github.com/pion/dtls/v3 v3.1.9 // indirect
	github.com/pion/ice/v4 v4.4.4 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.2.1 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/sctp v1.11.3
	github.com/pion/srtp/v3 v3.1.0 // indirect
	github.com/pion/stun/v4 v4.0.1 // indirect
	github.com/pion/transport/v5 v5.1.1
	github.com/pion/turn/v5 v5.1.2 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
)

// A one-change copy of pion/sctp (third_party/sctp/OPENFLUX_PATCH.md): the MTU of a data channel's packets can be set.
replace github.com/pion/sctp => ./third_party/sctp

replace gvisor.dev/gvisor => ./third_party/gvisor
