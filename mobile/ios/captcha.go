//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"strings"

	mobile "openflux-mobile"
)

// Checks (SmartCaptcha, a login) a carrier cannot pass on its own. The
// phone's own carriers report them as OpenFluxCaptchaPending; the exit's
// (it cannot pass them, there is no browser) as
// OpenFluxRemoteCaptchaPending: that page must be passed from the exit's
// address, so with the VPN up (every page goes through the exit) or through
// OpenFluxRemoteCaptchaProxy, and its cookies go back to the exit.

// OpenFluxCaptchaPending returns the page one of this phone's carriers
// needs passed, or "". Free with OpenFluxFreeString.
//
//export OpenFluxCaptchaPending
func OpenFluxCaptchaPending() *C.char {
	if mobile.PendingCaptchaProxy() != "" {
		return C.CString("")
	}
	return C.CString(mobile.PendingCaptchaURL())
}

// OpenFluxRemoteCaptchaPending returns the page the exit needs passed, or
// "". Free with OpenFluxFreeString.
//
//export OpenFluxRemoteCaptchaPending
func OpenFluxRemoteCaptchaPending() *C.char {
	if mobile.PendingCaptchaProxy() == "" {
		return C.CString("")
	}
	return C.CString(mobile.PendingCaptchaURL())
}

// OpenFluxRemoteCaptchaProxy returns the loopback HTTP proxy (host:port)
// whose connections leave through the exit, for a web view that can take a
// proxy (iOS 17: WKWebsiteDataStore.proxyConfigurations), or "". Free with
// OpenFluxFreeString.
//
//export OpenFluxRemoteCaptchaProxy
func OpenFluxRemoteCaptchaProxy() *C.char {
	return C.CString(mobile.PendingCaptchaProxy())
}

// OpenFluxCaptchaReason is "smartcaptcha" or "login" while a check is
// pending. Free with OpenFluxFreeString.
//
//export OpenFluxCaptchaReason
func OpenFluxCaptchaReason() *C.char {
	return C.CString(mobile.PendingCaptchaReason())
}

// OpenFluxApplyCaptchaCookies takes the cookies of a passed check (a
// Cookie header, "a=1; b=2") and hands them to the carrier that asked:
// this phone's, or the exit's (then they are sent to it). Returns the
// number of cookies taken, 0 on failure.
//
//export OpenFluxApplyCaptchaCookies
func OpenFluxApplyCaptchaCookies(cookies *C.char) C.int {
	if cookies == nil {
		return 0
	}
	header := C.GoString(cookies)
	if mobile.SubmitCaptchaCookies(header) != "" {
		return 0
	}
	return C.int(countCookies(header))
}

// OpenFluxOfferCaptchaCookies is OpenFluxApplyCaptchaCookies for the
// exit's check (the same call: the pending check knows whose it is).
//
//export OpenFluxOfferCaptchaCookies
func OpenFluxOfferCaptchaCookies(cookies *C.char) C.int {
	return OpenFluxApplyCaptchaCookies(cookies)
}

// OpenFluxCancelCaptcha drops the pending check (the user gave up); an
// exit's check stays quiet for a while.
//
//export OpenFluxCancelCaptcha
func OpenFluxCancelCaptcha() {
	mobile.CancelCaptcha()
}

// OpenFluxSetInitialCookies passes cookies got before starting (a check
// passed with the tunnel down) to the Yandex carriers of the next start.
//
//export OpenFluxSetInitialCookies
func OpenFluxSetInitialCookies(cookies *C.char) {
	s := ""
	if cookies != nil {
		s = C.GoString(cookies)
	}
	mobile.SetInitialCookies(s)
}

// OpenFluxSetCookieStore keeps passed checks' cookies in a file (the app's
// shared container) across starts. Returns 0, or 1 on error.
//
//export OpenFluxSetCookieStore
func OpenFluxSetCookieStore(path *C.char) C.int {
	if mobile.SetCookieStorePath(C.GoString(path)) != "" {
		return 1
	}
	return 0
}

func countCookies(header string) int {
	n := 0
	for _, part := range strings.Split(header, ";") {
		if name, _, ok := strings.Cut(strings.TrimSpace(part), "="); ok && name != "" {
			n++
		}
	}
	return n
}
