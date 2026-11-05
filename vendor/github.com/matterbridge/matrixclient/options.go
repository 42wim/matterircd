package matrixclient

import (
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Crypto adds end-to-end encryption support to a client. The e2ee subpackage
// provides an implementation, it is kept separate as it needs cgo (sqlite)
// and the goolm build tag (or libolm) which not every user wants.
type Crypto interface {
	mautrix.CryptoHelper

	Close() error
}

// DecryptFailureNotifier is optionally implemented by a Crypto that reports
// the messages it gave up decrypting, which are then never delivered. Without
// it the client expires the ordering data of undelivered encrypted messages by age.
type DecryptFailureNotifier interface {
	OnDecryptFailure(f func(ev *event.Event, err error))
}

// CryptoFactory creates a Crypto for mc. The returned Crypto is responsible for
// logging in with login, so that the device that owns the stored keys is reused.
type CryptoFactory func(mc *mautrix.Client, login *mautrix.ReqLogin) (Crypto, error)

// Option configures New.
type Option func(*options)

type options struct {
	crypto          CryptoFactory
	defaultProvider string
	nameResolved    func(roomID id.RoomID, name string)
	skipTLSVerify   bool
	timeout         time.Duration
	userAgent       string
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

// WithRoomNameResolved sets the callback for room names healed in the background,
// the counterpart of matterclient's OnChannelNameResolved. A room that was listed
// without a name or alias is reported once it has one, unless a RoomNameHandler
// rename already reported it. It is installed before syncing starts, and is
// called one at a time and must not block.
func WithRoomNameResolved(f func(roomID id.RoomID, name string)) Option {
	return func(o *options) { o.nameResolved = f }
}

// WithSkipTLSVerify disables verification of the homeserver certificate.
func WithSkipTLSVerify(skip bool) Option {
	return func(o *options) { o.skipTLSVerify = skip }
}

// WithTimeout sets the dial and TLS handshake timeout, 10 seconds by default.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithUserAgent sets the User-Agent header of the API requests.
func WithUserAgent(ua string) Option {
	return func(o *options) { o.userAgent = ua }
}
