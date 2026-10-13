package mobile

import "testing"

func TestOfferExitCookiesWithoutSession(t *testing.T) {
	setClientSession(nil, nil)
	if _, err := OfferExitCookies("vyandex", "Session_id=s"); err == nil {
		t.Fatal("want an error without a connected Session")
	}
	if _, err := OfferExitCookies("vyandex", ""); err == nil {
		t.Fatal("want an error without cookies")
	}
}
