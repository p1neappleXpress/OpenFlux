package manager

import "net/url"

// Auth URLs may carry tokens in the query, fragment or path. This value is
// only for logs; the original URL stays on the control/navigation path.
func redactedAuthURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "<invalid auth URL>"
	}
	pathClass := "/[path]"
	switch u.Path {
	case "/showcaptcha", "/showcaptchafast", "/auth":
		pathClass = u.Path
	case "", "/":
		pathClass = "/"
	}
	return u.Scheme + "://" + u.Host + pathClass
}
