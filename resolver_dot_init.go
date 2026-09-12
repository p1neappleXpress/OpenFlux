//go:build !ios

package main

// init ставит DoT-резолвер по умолчанию. На Android системный DNS часто
// подменяется оператором (наблюдали: ifconfig.me -> 240.0.1.72), поэтому
// резолвим имена через DNS-over-TLS, а не через getaddrinfo.
//
// iOS использует свой init() в export_ios.go — здесь он не нужен.
func init() {
	installDoTResolver()
}
