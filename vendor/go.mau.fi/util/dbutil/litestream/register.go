//go:build cgo

package litestream

import (
	"database/sql"
	"database/sql/driver"

	"github.com/mattn/go-sqlite3"
)

var Functions = make(map[string]any)

func setPragmas(conn *sqlite3.SQLiteConn, pragmas ...string) (err error) {
	for _, pragma := range pragmas {
		if _, err = conn.Exec(pragma, []driver.Value{}); err != nil {
			return
		}
	}
	return
}

func registerFuncsAndSetTrace(conn *sqlite3.SQLiteConn) (err error) {
	for name, fn := range Functions {
		err = conn.RegisterFunc(name, fn, true)
		if err != nil {
			return
		}
	}
	err = DoSetTrace(conn)
	return
}

func init() {
	sql.Register("litestream", &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) (err error) {
			if err = registerFuncsAndSetTrace(conn); err != nil {
				return
			}
			if err = conn.SetFileControlInt("main", sqlite3.SQLITE_FCNTL_PERSIST_WAL, 1); err != nil {
				return
			}
			err = setPragmas(
				conn,
				"PRAGMA foreign_keys = ON",
				"PRAGMA journal_mode = WAL",
				"PRAGMA wal_autocheckpoint = 0",
				"PRAGMA synchronous = NORMAL",
				"PRAGMA busy_timeout = 5000",
			)
			return
		},
	})

	sql.Register("sqlite3-fk-wal", &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) (err error) {
			if err = registerFuncsAndSetTrace(conn); err != nil {
				return
			}
			err = setPragmas(
				conn,
				"PRAGMA foreign_keys = ON",
				"PRAGMA journal_mode = WAL",
				"PRAGMA synchronous = NORMAL",
				"PRAGMA busy_timeout = 5000",
			)
			return
		},
	})

	sql.Register("sqlite3-fk-wal-fullsync", &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) (err error) {
			if err = registerFuncsAndSetTrace(conn); err != nil {
				return
			}
			err = setPragmas(
				conn,
				"PRAGMA foreign_keys = ON",
				"PRAGMA journal_mode = WAL",
				"PRAGMA synchronous = FULL",
				"PRAGMA busy_timeout = 5000",
			)
			return
		},
	})
}
