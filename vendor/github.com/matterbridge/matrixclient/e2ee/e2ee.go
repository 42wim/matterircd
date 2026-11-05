// Package e2ee enables end-to-end encrypted rooms for matrixclient.
//
// Build with the goolm tag (pure Go olm) or have libolm installed, and with
// cgo enabled for the sqlite database holding the keys:
//
//	go build -tags goolm
package e2ee

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

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

// ownDevice returns our device from the local crypto store. It deliberately
// avoids FetchKeys, which writes device and cross-signing data without
// synchronizing with the machine's own sync handling.
func (h helper) ownDevice(ctx context.Context, m *crypto.OlmMachine) (*id.Device, error) {
	dev, err := m.CryptoStore.GetDevice(ctx, h.client.UserID, h.client.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("get own device: %w", err)
	}

	if dev == nil {
		return nil, fmt.Errorf("own device %s not in crypto store", h.client.DeviceID)
	}

	if dev.SigningKey.Fingerprint() != m.Fingerprint() {
		return nil, fmt.Errorf("own device %s signing key does not match local key", h.client.DeviceID)
	}

	return dev, nil
}

// DeviceVerified implements matrixclient.DeviceVerifier.
func (h helper) DeviceVerified(ctx context.Context) (bool, error) {
	m := h.Machine()
	if m == nil {
		return false, nil
	}

	dev, err := h.ownDevice(ctx, m)
	if err != nil {
		return false, err
	}

	trust, err := m.ResolveTrustContext(ctx, dev)
	if err != nil {
		return false, fmt.Errorf("resolve own device trust: %w", err)
	}

	// ResolveTrustContext discards IsUserTrusted errors and falls back to TOFU or
	// untrusted, so retry it to surface errors and honor a trusted result.
	if trust == id.TrustStateCrossSignedTOFU || trust == id.TrustStateCrossSignedUntrusted {
		var trusted bool

		trusted, err = m.IsUserTrusted(ctx, h.client.UserID)
		if err != nil {
			return false, fmt.Errorf("check own user trust: %w", err)
		}

		if trusted {
			trust = id.TrustStateCrossSignedVerified
		}
	}

	switch trust {
	case id.TrustStateVerified, id.TrustStateCrossSignedTOFU, id.TrustStateCrossSignedVerified:
		return true, nil
	default:
		return false, nil
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

		return helper{CryptoHelper: ch, client: mc}, nil
	})
}
