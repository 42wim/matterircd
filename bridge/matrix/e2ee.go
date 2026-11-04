//go:build goolm

package matrix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/matterbridge/matrixclient"
	"github.com/matterbridge/matrixclient/e2ee"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
)

func storeHash(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:8])
}

// storeName names the key database of an account by hash to keep the path safe.
// Token logins use the stable user and device the token belongs to, as the
// token itself changes when replaced.
func (m *Matrix) storeName(cred bridge.Credentials) (string, error) {
	if cred.Token == "" {
		return storeHash(cred.Login + "@" + cred.Server), nil
	}

	mc, err := mautrix.NewClient(cred.Server, "", cred.Token)
	if err != nil {
		return "", fmt.Errorf("matrix: cannot look up token owner: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := mc.Whoami(ctx)
	if err != nil {
		return "", fmt.Errorf("matrix: cannot look up token owner: %w", err)
	}

	return storeHash(resp.UserID.String() + "/" + resp.DeviceID.String() + "@" + cred.Server), nil
}

func (m *Matrix) encryptionOptions(cred bridge.Credentials) ([]matrixclient.Option, error) {
	mxCfg := m.cfg.Matrix()
	if mxCfg.EncryptionDir == "" || mxCfg.EncryptionPickleKey == "" {
		logger.Info("matrix: end-to-end encryption NOT ENABLED - EncryptionDir and EncryptionPickleKey must both be set")

		return nil, nil
	}

	err := os.MkdirAll(mxCfg.EncryptionDir, 0o700)
	if err != nil {
		logger.Errorf("matrix: encryption disabled, cannot create %s: %s", mxCfg.EncryptionDir, err)

		return nil, nil
	}

	name, err := m.storeName(cred)
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(mxCfg.EncryptionDir, name+".db")

	logger.Infof("matrix: end-to-end encryption enabled, key database %s", dbPath)
	m.e2ee.Store(true)

	return []matrixclient.Option{e2ee.Factory(e2ee.Config{
		DBPath:    dbPath,
		PickleKey: []byte(mxCfg.EncryptionPickleKey),
		OnDecryptError: func(ev *event.Event, err error) {
			logger.Warnf("matrix: cannot decrypt %s in %s: %s", ev.ID, ev.RoomID, err)
		},
	})}, nil
}
