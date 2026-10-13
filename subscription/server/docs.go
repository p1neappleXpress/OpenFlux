package server

import (
	"context"
	"errors"
)

// DocFactory makes a document for one owner at one carrier (vyandex, mailru,
// boards) and returns its url plus the cookies a node reads it with. It is the
// slot for the office accounts: without them there are no documents.
type DocFactory interface {
	Make(ctx context.Context, carrier, owner string) (url, cookies string, err error)
}

// errNoDocAccount is what a factory without an office account says.
var errNoDocAccount = errors.New("нет служебного аккаунта")

// accountDocFactory uses the office accounts from the config. Until those
// accounts exist it always reports errNoDocAccount and callers drop the
// carrier, so a channel is still created with what does work.
type accountDocFactory struct{}

func (accountDocFactory) Make(context.Context, string, string) (string, string, error) {
	return "", "", errNoDocAccount
}

// docCarrier reports whether a transport type needs a document from a factory.
func docCarrier(typ string) bool {
	switch typ {
	case "vyandex", "yandex", "mailru", "boards":
		return true
	}
	return false
}
