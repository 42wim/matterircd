//go:build cgo && sqlite_trace

package litestream

import (
	"fmt"
	"time"

	"github.com/mattn/go-sqlite3"
)

func traceCallback(info sqlite3.TraceInfo) int {
	switch info.EventCode {
	case sqlite3.TraceStmt:
		fmt.Println(info.ExpandedSQL)
		fmt.Println("-------------------------------------------------------------")
	default:
		fmt.Print("exectime: ", time.Duration(info.RunTimeNanosec).String())
		if info.AutoCommit {
			fmt.Print(" - autocommit")
		} else {
			fmt.Print(" - transaction")
		}
		if info.DBError.Code != 0 || info.DBError.ExtendedCode != 0 {
			fmt.Printf(" - error %#v", info.DBError)
		}
		fmt.Println()
	}
	return 0
}

func DoSetTrace(conn *sqlite3.SQLiteConn) error {
	return conn.SetTrace(&sqlite3.TraceConfig{
		Callback:        traceCallback,
		EventMask:       sqlite3.TraceStmt | sqlite3.TraceProfile | sqlite3.TraceRow | sqlite3.TraceClose,
		WantExpandedSQL: true,
	})
}
