package yandex

import (
	"openflux/transport"
)

var _ transport.CookieExchanger = (*YandexDocsTransport)(nil)
