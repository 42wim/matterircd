// Package log provides a common interface for logging libraries.
package log

import "io"

// Level specifies a level of verbosity. The available levels are the eight
// severities described in RFC 5424 and none.
type Level int8

const (
	None Level = iota - 1
	Emergency
	Alert
	Critical
	Error
	Warning
	Notice
	Info
	Debug
)

// Logger is a common interface for logging libraries.
type Logger interface {
	// Emergency logs with an emergency level.
	Emergency(args ...interface{})

	// Emergencyf logs with an emergency level.
	// Arguments are handled in the manner of fmt.Printf.
	Emergencyf(format string, args ...interface{})

	// Alert logs with an alert level.
	Alert(args ...interface{})

	// Alertf logs with an alert level.
	// Arguments are handled in the manner of fmt.Printf.
	Alertf(format string, args ...interface{})

	// Critical logs with a critical level.
	Critical(args ...interface{})

	// Criticalf logs with a critical level.
	// Arguments are handled in the manner of fmt.Printf.
	Criticalf(format string, args ...interface{})

	// Error logs with an error level.
	Error(args ...interface{})

	// Errorf logs with an error level.
	// Arguments are handled in the manner of fmt.Printf.
	Errorf(format string, args ...interface{})

	// Warning logs with a warning level.
	Warning(args ...interface{})

	// Warningf logs with a warning level.
	// Arguments are handled in the manner of fmt.Printf.
	Warningf(format string, args ...interface{})

	// Notice logs with a notice level.
	Notice(args ...interface{})

	// Noticef logs with a notice level.
	// Arguments are handled in the manner of fmt.Printf.
	Noticef(format string, args ...interface{})

	// Info logs with an info level.
	Info(args ...interface{})

	// Infof logs with an info level.
	// Arguments are handled in the manner of fmt.Printf.
	Infof(format string, args ...interface{})

	// Debug logs with a debug level.
	Debug(args ...interface{})

	// Debugf logs with a debug level.
	// Arguments are handled in the manner of fmt.Printf.
	Debugf(format string, args ...interface{})

	// Log logs at the level passed in argument.
	Log(level Level, args ...interface{})

	// Logf logs at the level passed in argument.
	// Arguments are handled in the manner of fmt.Printf.
	Logf(level Level, format string, args ...interface{})

	// LogEmergency returns true if the log level is at emergency or lower.
	LogEmergency() bool

	// LogAlert returns true if the log level is at alert or lower.
	LogAlert() bool

	// LogCritical returns true if the log level is at critical or lower.
	LogCritical() bool

	// LogError returns true if the log level is at error or lower.
	LogError() bool

	// LogWarning returns true if the log level is at warning or lower.
	LogWarning() bool

	// LogNotice returns true if the log level is at notice or lower.
	LogNotice() bool

	// LogInfo returns true if the log level is at info or debug.
	LogInfo() bool

	// LogDebug returns true only if the log level is at debug.
	LogDebug() bool

	// LogLevel returns true if the log level is at or below the level argument.
	LogLevel(level Level) bool

	io.Closer
}
