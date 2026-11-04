package matrixclient

import (
	"maunium.net/go/mautrix"
)

// Crypto adds end-to-end encryption support to a client. The e2ee subpackage
// provides an implementation, it is kept separate as it needs cgo (sqlite)
// and the goolm build tag (or libolm) which not every user wants.
type Crypto interface {
	mautrix.CryptoHelper

	Close() error
}

// CryptoFactory creates a Crypto for mc. The returned Crypto is responsible for
// logging in with login, so that the device that owns the stored keys is reused.
type CryptoFactory func(mc *mautrix.Client, login *mautrix.ReqLogin) (Crypto, error)

// Option configures New.
type Option func(*options)

type options struct {
	crypto          CryptoFactory
	defaultProvider string
}

// WithCrypto enables end-to-end encryption, see the e2ee subpackage.
func WithCrypto(f CryptoFactory) Option {
	return func(o *options) { o.crypto = f }
}

// WithDefaultProvider sets the homeserver that is left out of room names: aliases on
// it are reported without the "/provider" suffix by Rooms, and Join appends it to
// aliases that have none.
func WithDefaultProvider(provider string) Option {
	return func(o *options) { o.defaultProvider = provider }
}
