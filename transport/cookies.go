package transport

// CookieExchanger is implemented by transports that carry cookies and can
// refresh or accept them at runtime.
//
// The exit node uses FetchCookies when a client asks for a refresh; both
// sides use ApplyCookies when the peer hands over a fresh jar.
//
// Implementations must be safe for concurrent use: FetchCookies and
// ApplyCookies may be called from the NegotiatedTransport control goroutine
// while the transport's own receive loop is running.
type CookieExchanger interface {
	// FetchCookies returns the transport's current cookie jar as
	// name -> value. The exit node calls this to answer a
	// SubtypeCookiesRequest from the client.
	FetchCookies() (map[string]string, error)

	// ApplyCookies replaces the transport's cookie jar with the provided
	// values. It is called on the exit node when a client sends
	// SubtypeCookiesOffer, and on the client when the exit replies with
	// SubtypeCookiesResponse.
	//
	// ApplyCookies must be idempotent: the same map applied twice yields
	// the same state.
	ApplyCookies(jar map[string]string) error
}
