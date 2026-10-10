package yandex

import (
	"errors"
	"net/url"
)

// AuthRequiredError preserves the browser redirect separately from the
// document used to identify the transport. URL may contain credentials;
// Error deliberately reports only the sentinel cause.
type AuthRequiredError struct {
	Cause error
	URL   string
}

func (e *AuthRequiredError) Error() string { return e.Cause.Error() }
func (e *AuthRequiredError) Unwrap() error { return e.Cause }

func authRedirectError(cause error, currentURL, location string) error {
	base, err := url.Parse(currentURL)
	if err != nil {
		return cause
	}
	ref, err := url.Parse(location)
	if err != nil {
		return cause
	}
	resolved := base.ResolveReference(ref)
	if (resolved.Scheme != "http" && resolved.Scheme != "https") || resolved.Host == "" {
		return cause
	}
	return &AuthRequiredError{Cause: cause, URL: resolved.String()}
}

func authRequiredURL(err error, documentURL string) string {
	var required *AuthRequiredError
	if errors.As(err, &required) && required.URL != "" {
		return required.URL
	}
	return documentURL
}
