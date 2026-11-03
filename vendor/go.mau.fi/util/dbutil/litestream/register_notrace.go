//go:build cgo && !sqlite_trace

package litestream

import (
	"github.com/mattn/go-sqlite3"
)

func DoSetTrace(conn *sqlite3.SQLiteConn) error {
	return nil
}
