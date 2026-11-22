//go:build !cgo

package e2ee

import (
	"database/sql"
	"net/url"
	"path/filepath"

	_ "github.com/ncruces/go-sqlite3/driver"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
)

func newCryptoHelper(mc *mautrix.Client, cfg Config) (*cryptohelper.CryptoHelper, error) {
	dsn := (&url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(cfg.DBPath),
		RawQuery: url.Values{
			"_txlock": {"immediate"},
			"_pragma": {
				"busy_timeout(5000)",
				"foreign_keys(ON)",
				"journal_mode(WAL)",
				"synchronous(NORMAL)",
			},
		}.Encode(),
	}).String()

	rawDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	db, err := dbutil.NewWithDB(rawDB, "sqlite3")
	if err != nil {
		_ = rawDB.Close()

		return nil, err
	}

	ch, err := cryptohelper.NewCryptoHelper(mc, cfg.PickleKey, db)
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	return ch, nil
}
