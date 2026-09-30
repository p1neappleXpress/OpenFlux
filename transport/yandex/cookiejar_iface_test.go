package yandex

import (
	"github.com/p1neappleXpress/OpenFlux/transport"
)

var _ transport.CookieExchanger = (*YandexDocsTransport)(nil)
