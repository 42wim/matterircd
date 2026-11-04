// Package e2ee enables end-to-end encrypted rooms for matrixclient.
//
// Build with the goolm tag (pure Go olm) or have libolm installed, and with
// cgo enabled for the sqlite database holding the keys:
//
//	go build -tags goolm
package e2ee

import (
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"

	"github.com/matterbridge/matrixclient"
)

// Config configures encryption.
type Config struct {
	// DBPath is the sqlite database storing the device keys and sessions. Keep
	// it: losing it makes the device unable to decrypt earlier messages.
	DBPath string
	// PickleKey encrypts the keys in the database and must stay the same.
	PickleKey []byte
	// OnDecryptError is called for messages that could not be decrypted. Optional.
	OnDecryptError func(ev *event.Event, err error)
}

// helper reports terminal decryption failures to the client, so it can drop
// the ordering data kept for the undelivered message.
type helper struct {
	*cryptohelper.CryptoHelper
}

func (h helper) OnDecryptFailure(f func(ev *event.Event, err error)) {
	prev := h.DecryptErrorCallback

	h.DecryptErrorCallback = func(ev *event.Event, err error) {
		f(ev, err)

		if prev != nil {
			prev(ev, err)
		}
	}
}

// Factory returns the option to pass to matrixclient.New.
func Factory(cfg Config) matrixclient.Option {
	return matrixclient.WithCrypto(func(mc *mautrix.Client, login *mautrix.ReqLogin) (matrixclient.Crypto, error) {
		ch, err := cryptohelper.NewCryptoHelper(mc, cfg.PickleKey, cfg.DBPath)
		if err != nil {
			return nil, err
		}

		if login != nil {
			ch.LoginAs = login
		}

		if cfg.OnDecryptError != nil {
			ch.DecryptErrorCallback = cfg.OnDecryptError
		}

		return helper{ch}, nil
	})
}
