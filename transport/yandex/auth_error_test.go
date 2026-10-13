package yandex

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAuthErrorFallbackAndPrivateFormatting(t *testing.T) {
	const document = "https://docs.example/original"
	for _, cause := range []error{ErrCaptchaRequired, ErrLoginRequired} {
		if authRequiredURL(fmt.Errorf("wrapped: %w", cause), document) != document {
			t.Fatal("fallback changed")
		}
		err := authRedirectError(cause, document, "/showcaptcha?must-stay-private#fragment")
		wrapped := fmt.Errorf("auth: %w", err)
		if !errors.Is(wrapped, cause) {
			t.Fatal("errors.Is lost")
		}
		if strings.Contains(wrapped.Error(), "must-stay-private") || strings.Contains(wrapped.Error(), "showcaptcha") {
			t.Fatal("error string exposes URL")
		}
		if authRequiredURL(authRedirectError(cause, document, ":%invalid"), document) != document {
			t.Fatal("invalid Location must fall back")
		}
	}
}

func TestAuthRedirectRejectsInvalidTargets(t *testing.T) {
	const doc = "https://docs.example/document"
	for _, location := range []string{":%invalid", "javascript:showcaptcha", "data:text/plain,showcaptcha"} {
		err := authRedirectError(ErrCaptchaRequired, doc, location)
		if !errors.Is(err, ErrCaptchaRequired) || authRequiredURL(err, doc) != doc {
			t.Fatal("invalid target must preserve sentinel and document fallback")
		}
	}
}

func TestAuthRedirectResolvesHTTPSLocations(t *testing.T) {
	const current = "https://docs.example/nested/document"
	for location, want := range map[string]string{
		"/showcaptcha?token=SYNTHETIC#fragment": "https://docs.example/showcaptcha?token=SYNTHETIC#fragment",
		"../showcaptcha?token=SYNTHETIC":        "https://docs.example/showcaptcha?token=SYNTHETIC",
		"//captcha.example/showcaptcha?token=X": "https://captcha.example/showcaptcha?token=X",
	} {
		err := fmt.Errorf("wrapped: %w", authRedirectError(ErrCaptchaRequired, current, location))
		var auth *AuthRequiredError
		if !errors.As(err, &auth) || auth.URL != want || authRequiredURL(err, current) != want || !errors.Is(err, ErrCaptchaRequired) {
			t.Fatal("resolved HTTPS challenge or wrapped cause was lost")
		}
	}
}
