//go:build goolm

package matrix

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/42wim/matterircd/bridge"
	"github.com/matterbridge/matrixclient"
	"github.com/matterbridge/matrixclient/e2ee"
	"maunium.net/go/mautrix/event"
)

func (m *Matrix) encryptionOptions(cred bridge.Credentials) []matrixclient.Option {
	mxCfg := m.cfg.Matrix()
	if mxCfg.EncryptionDir == "" || mxCfg.EncryptionPickleKey == "" {
		return nil
	}

	err := os.MkdirAll(mxCfg.EncryptionDir, 0o700)
	if err != nil {
		logger.Errorf("matrix: encryption disabled, cannot create %s: %s", mxCfg.EncryptionDir, err)

		return nil
	}

	// One key database per account, named by hash to keep the path safe.
	account := cred.Login
	if account == "" {
		// A token belongs to one device, which the key database is bound to.
		account = cred.Token
	}

	sum := sha256.Sum256([]byte(account + "@" + cred.Server))
	dbPath := filepath.Join(mxCfg.EncryptionDir, hex.EncodeToString(sum[:8])+".db")

	return []matrixclient.Option{e2ee.Factory(e2ee.Config{
		DBPath:    dbPath,
		PickleKey: []byte(mxCfg.EncryptionPickleKey),
		OnDecryptError: func(ev *event.Event, err error) {
			logger.Warnf("matrix: cannot decrypt %s in %s: %s", ev.ID, ev.RoomID, err)
		},
	})}
}
