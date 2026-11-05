// Package e2ee enables end-to-end encrypted rooms for matrixclient.
//
// Build with the goolm tag (pure Go olm) or have libolm installed, and with
// cgo enabled for the sqlite database holding the keys:
//
//	go build -tags goolm
package e2ee

import (
	"context"

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

	client *mautrix.Client
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

// Fingerprint implements matrixclient.FingerprintProvider.
func (h helper) Fingerprint() string {
	m := h.Machine()
	if m == nil {
		return ""
	}

	return m.Fingerprint()
}

// DeviceVerified implements matrixclient.DeviceVerifier.
func (h helper) DeviceVerified(ctx context.Context) bool {
	m := h.Machine()
	if m == nil {
		return false
	}

	// GetOrFetchDevice would return the cached device, whose trust may predate a
	// verification done from another client, so always refresh from the server.
	dev, ok := m.LoadDevices(ctx, h.client.UserID)[h.client.DeviceID]
	if !ok {
		return false
	}

	return m.IsDeviceTrusted(ctx, dev)
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

		return helper{CryptoHelper: ch, client: mc}, nil
	})
}
