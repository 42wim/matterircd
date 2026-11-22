//go:build cgo

package e2ee

import (
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
)

func newCryptoHelper(mc *mautrix.Client, cfg Config) (*cryptohelper.CryptoHelper, error) {
	return cryptohelper.NewCryptoHelper(mc, cfg.PickleKey, cfg.DBPath)
}
