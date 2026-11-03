//go:build !goolm

package matrix

import (
	"github.com/42wim/matterircd/bridge"
	"github.com/matterbridge/matrixclient"
)

func (m *Matrix) encryptionOptions(bridge.Credentials) ([]matrixclient.Option, error) {
	if m.cfg.Matrix().EncryptionDir != "" {
		logger.Warn("matrix: EncryptionDir is set but this build has no encryption support, build with -tags goolm")
	}

	return nil, nil
}
