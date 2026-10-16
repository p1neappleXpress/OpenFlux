package main

// classicCompatible reports whether a Session also speaks the classic
// layering. A client falls back to it while the exit does not answer the
// handshake and upgrades once it does: a classic setup (--transport=X with
// a key), or a single-carrier --transports/.conf client. An exit serves
// classic clients next to Session ones whatever defines its carriers, so
// a wizard node (.conf) reaches builds that only speak classic, such as
// the iOS app. Only --negotiate is strict; without a key only classic is
// possible and no Session runs.
func classicCompatible(role string, configuredSession, negotiate, hasKey bool, carriers int) bool {
	switch {
	case role != roleClient && role != roleExit:
		return false
	case !configuredSession:
		return hasKey
	case negotiate:
		return false
	case role == roleExit:
		return true
	}
	return carriers == 1
}
