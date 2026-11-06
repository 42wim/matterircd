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

// Init initializes the crypto helper and, before syncing starts, tracks our own
// user so a brand-new store gets our device and cross-signing keys.
func (h helper) Init(ctx context.Context) error {
	err := h.CryptoHelper.Init(ctx)
	if err != nil {
		return err
	}

	// Room state is only kept in memory, so resuming from a persisted
	// next_batch would deliver no rooms. Keys and sessions are unaffected.
	err = h.client.Store.SaveNextBatch(ctx, h.client.UserID, "")
	if err != nil {
		return fmt.Errorf("reset sync token: %w", err)
	}

	m := h.Machine()
	if m == nil {
		return nil
	}

	// The remaining steps only prime state for DeviceVerified, which reports
	// the real error later, so failures must not abort login.
	h.primeOwnKeys(ctx, m)

	return nil
}

func (h helper) primeOwnKeys(ctx context.Context, m *crypto.OlmMachine) {
	log := h.client.Log

	tracked, err := m.CryptoStore.FilterTrackedUsers(ctx, []id.UserID{h.client.UserID})
	if err != nil {
		log.Warn().Err(err).Msg("Failed to check own user tracking")

		return
	}

	if len(tracked) > 0 {
		return
	}

	// Safe here: the sync loop has not started, so nothing else writes the keys.
	_, err = m.FetchKeys(ctx, []id.UserID{h.client.UserID}, true)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch own keys")

		return
	}

	// FetchKeys logs per-server and storage failures without returning them.
	tracked, err = m.CryptoStore.FilterTrackedUsers(ctx, []id.UserID{h.client.UserID})
	if err != nil {
		log.Warn().Err(err).Msg("Failed to check own user tracking")

		return
	}

	if len(tracked) == 0 {
		log.Warn().Stringer("user_id", h.client.UserID).Msg("Own user not tracked after fetching keys")

		return
	}

	_, err = h.ownDevice(ctx, m)
	if err != nil {
		log.Warn().Err(err).Msg("Own device check failed")
	}
}

// DeviceVerified implements matrixclient.DeviceVerifier. It resolves trust from
// the crypto store only: the machine's cached resolvers (ResolveTrustContext,
// IsUserTrusted) mutate unsynchronized fields shared with the sync handling.
func (h helper) DeviceVerified(ctx context.Context) (bool, error) {
	m := h.Machine()
	if m == nil {
		return false, nil
	}

	dev, err := h.ownDevice(ctx, m)
	if err != nil {
		return false, err
	}

	// The local store can retain a device (and its trust) that was deleted
	// server-side. This read-only request does not touch the crypto store.
	_, err = h.client.GetDeviceInfo(ctx, h.client.DeviceID)
	if err != nil {
		return false, fmt.Errorf("get own device %s from server: %w", h.client.DeviceID, err)
	}

	switch dev.Trust {
	case id.TrustStateVerified:
		return true, nil
	case id.TrustStateBlacklisted:
		return false, nil
	}

	keys, err := m.CryptoStore.GetCrossSigningKeys(ctx, h.client.UserID)
	if err != nil {
		return false, fmt.Errorf("get own cross-signing keys: %w", err)
	}

	msk, ssk, usk := keys[id.XSUsageMaster], keys[id.XSUsageSelfSigning], keys[id.XSUsageUserSigning]
	if msk.Key == "" || ssk.Key == "" {
		return false, nil
	}

	// Mirrors the resolver's "Own master key has changed" check, using the
	// locally held keys rather than the machine's unsynchronized pubkey cache.
	// mautrix only ever replaces the CrossSigningKeys pointer (import/generate),
	// never mutates it, so a single read yields a consistent snapshot.
	if own := m.CrossSigningKeys; own != nil && own.MasterKey.PublicKey() != msk.Key {
		return false, nil
	}

	user := h.client.UserID

	signed, err := m.CryptoStore.IsKeySignedBy(ctx, user, ssk.Key, user, msk.Key)
	if err != nil {
		return false, fmt.Errorf("check self-signing key signature: %w", err)
	}

	if !signed {
		return false, nil
	}

	signed, err = m.CryptoStore.IsKeySignedBy(ctx, user, dev.SigningKey, user, ssk.Key)
	if err != nil {
		return false, fmt.Errorf("check own device signature: %w", err)
	}

	if !signed {
		return false, nil
	}

	// Mirrors IsUserTrusted for our own user: the master key must be signed by
	// this device and the user-signing key by the master key.
	trusted := usk.Key != ""
	if trusted {
		trusted, err = m.CryptoStore.IsKeySignedBy(ctx, user, msk.Key, user, dev.SigningKey)
		if err != nil {
			return false, fmt.Errorf("check master key signature: %w", err)
		}
	}

	if trusted {
		trusted, err = m.CryptoStore.IsKeySignedBy(ctx, user, usk.Key, user, msk.Key)
		if err != nil {
			return false, fmt.Errorf("check user-signing key signature: %w", err)
		}
	}

	// Trust on first use, unless the master key changed since first seen.
	return trusted || msk.Key == msk.First, nil
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
