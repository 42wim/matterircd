// Package buflog provides a buffered logging class that accumulates logs in
// memory until the flush threshold is reached which release stored logs, the
// buffered logger then act as a normal logger.
//
// Basic example:
//
//     logger := buflog.New(os.Stdout, log.Info, log.Error)
//     logger.Info("Connecting to the server...")   // Outputs nothing
//     logger.Error("Connection failed")            // Outputs both lines
package buflog

import (
	"bufio"
	"io"

	"github.com/alexcesaro/log"
	"github.com/alexcesaro/log/golog"
)

// A Logger represents an active buffered logging object.
type Logger struct {
	*golog.Logger
	flush  log.Level
	buffer *bufio.Writer
}

// New creates a new Logger. The out variable sets the destination to which
// log data will be written. The threshold variable defines the level under
// which logging will be ignored. The flushThreshold variable defines the
// level above which logging will be output.
func New(out io.Writer, threshold log.Level, flushThreshold log.Level) *Logger {
	logger := &Logger{
		Logger: golog.New(out, threshold),
		flush:  flushThreshold,
		buffer: bufio.NewWriter(out),
	}
	replaceWriter(logger)

	return logger
}

func replaceWriter(logger *Logger) {
	oldWriter := logger.Logger.Writer

	logger.Logger.Writer = func(out io.Writer, logLine []byte, level log.Level) {
		if level > logger.flush {
			logger.buffer.Write(logLine)
		} else {
			logger.buffer.Flush()
			logger.Logger.Writer = oldWriter
			logger.Logger.Writer(out, logLine, level)
		}
	}
}
