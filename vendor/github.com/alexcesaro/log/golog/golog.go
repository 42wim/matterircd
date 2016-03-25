// Package golog provides a customizable logging class which can be used as a
// standalone or as a building block for other loggers.
//
// Basic example:
//
//     logger := golog.New(os.Stdout, log.Info)
//     logger.Info("Connecting to the server...")
//     logger.Errorf("Connection failed: %q", err)
//
// Will output:
//
//     2014-04-02 18:09:15.862 INFO Connecting to the API...
//     2014-04-02 18:10:14.347 ERROR Connection failed (Server is unavailable).
//
// Log*() functions can be used to avoid evaluating arguments when it is
// expensive and unnecessary:
//
//     logger.Debug("Memory usage: %s", getMemoryUsage())
//     if logger.LogDebug() { logger.Debug("Memory usage: %s", getMemoryUsage()) }
//
// If debug logging is off getMemoryUsage() will be executed on the first line
// while it will not be executed on the second line.
package golog

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/alexcesaro/log"
)

// A Logger represents an active logging object that generates lines of output
// to an io.Writer. Each logging operation makes a single call to the Writer's
// Write method. A Logger can be used simultaneously from multiple goroutines;
// it guarantees to serialize access to the Writer.
type Logger struct {
	out             io.Writer
	threshold       log.Level
	writeMutex      sync.Mutex
	Formatter       func(*bytes.Buffer, log.Level, ...interface{})
	Writer          func(io.Writer, []byte, log.Level)
	bufferList      *buffer
	bufferListMutex sync.Mutex
}

// New creates a new Logger. The out variable sets the destination to which log
// data will be written. The threshold variable defines the level under which
// logging will be ignored.
func New(out io.Writer, threshold log.Level) *Logger {
	return &Logger{
		out:       out,
		threshold: threshold,
		Formatter: defaultFormater,
		Writer:    defaultWriter,
	}
}

// Emergency logs with an emergency level.
func (logger *Logger) Emergency(args ...interface{}) {
	logger.output(log.Emergency, args...)
}

// Emergencyf logs with an emergency level.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Emergencyf(format string, args ...interface{}) {
	logger.output(log.Emergency, fmt.Sprintf(format, args...))
}

// Alert logs with an alert level.
func (logger *Logger) Alert(args ...interface{}) {
	logger.output(log.Alert, args...)
}

// Alertf logs with an alert level.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Alertf(format string, args ...interface{}) {
	logger.output(log.Alert, fmt.Sprintf(format, args...))
}

// Critical logs with a critical level.
func (logger *Logger) Critical(args ...interface{}) {
	logger.output(log.Critical, args...)
}

// Criticalf logs with a critical level.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Criticalf(format string, args ...interface{}) {
	logger.output(log.Critical, fmt.Sprintf(format, args...))
}

// Error logs with an error level.
func (logger *Logger) Error(args ...interface{}) {
	logger.output(log.Error, args...)
}

// Errorf logs with an error level.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Errorf(format string, args ...interface{}) {
	logger.output(log.Error, fmt.Sprintf(format, args...))
}

// Warning logs with a warning level.
func (logger *Logger) Warning(args ...interface{}) {
	logger.output(log.Warning, args...)
}

// Warningf logs with a warning level.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Warningf(format string, args ...interface{}) {
	logger.output(log.Warning, fmt.Sprintf(format, args...))
}

// Notice logs with a notice level.
func (logger *Logger) Notice(args ...interface{}) {
	logger.output(log.Notice, args...)
}

// Noticef logs with a notice level.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Noticef(format string, args ...interface{}) {
	logger.output(log.Notice, fmt.Sprintf(format, args...))
}

// Info logs with an info level.
func (logger *Logger) Info(args ...interface{}) {
	logger.output(log.Info, args...)
}

// Infof logs with an info level.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Infof(format string, args ...interface{}) {
	logger.output(log.Info, fmt.Sprintf(format, args...))
}

// Debug logs with a debug level.
func (logger *Logger) Debug(args ...interface{}) {
	logger.output(log.Debug, args...)
}

// Debugf logs with a debug level.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Debugf(format string, args ...interface{}) {
	logger.output(log.Debug, fmt.Sprintf(format, args...))
}

// Log logs at the level passed in argument.
func (logger *Logger) Log(level log.Level, args ...interface{}) {
	logger.output(level, args...)
}

// Logf logs at the level passed in argument.
// Arguments are handled in the manner of fmt.Printf.
func (logger *Logger) Logf(level log.Level, format string, args ...interface{}) {
	logger.output(level, fmt.Sprintf(format, args...))
}

// LogEmergency returns true if the log level is at emergency or lower.
func (logger *Logger) LogEmergency() bool {
	return logger.threshold >= log.Emergency
}

// LogAlert returns true if the log level is at alert or lower.
func (logger *Logger) LogAlert() bool {
	return logger.threshold >= log.Alert
}

// LogCritical returns true if the log level is at critical or lower.
func (logger *Logger) LogCritical() bool {
	return logger.threshold >= log.Critical
}

// LogError returns true if the log level is at error or lower.
func (logger *Logger) LogError() bool {
	return logger.threshold >= log.Error
}

// LogWarning returns true if the log level is at warning or lower.
func (logger *Logger) LogWarning() bool {
	return logger.threshold >= log.Warning
}

// LogNotice returns true if the log level is at notice or lower.
func (logger *Logger) LogNotice() bool {
	return logger.threshold >= log.Notice
}

// LogInfo returns true if the log level is at info or debug.
func (logger *Logger) LogInfo() bool {
	return logger.threshold >= log.Info
}

// LogDebug returns true only if the log level is at debug.
func (logger *Logger) LogDebug() bool {
	return logger.threshold >= log.Debug
}

// LogLevel returns true if the log level is at or below the level argument.
func (logger *Logger) LogLevel(level log.Level) bool {
	return logger.threshold >= level
}

// Close does nothing and is just here so that Logger satisfies the log.Logger
// interface.
func (logger *Logger) Close() error { return nil }

func (logger *Logger) output(level log.Level, args ...interface{}) {
	if level > logger.threshold {
		return
	}

	buffer := logger.getBuffer()
	logger.Formatter(&buffer.Buffer, level, args...)

	logger.writeMutex.Lock()
	logger.Writer(logger.out, buffer.Bytes(), level)
	logger.writeMutex.Unlock()

	logger.putBuffer(buffer)
}

var defaultFormater = func(buffer *bytes.Buffer, level log.Level, args ...interface{}) {
	addTimestamp(buffer)
	addLevel(buffer, level)
	addMessage(buffer, args...)
}

// Avoid Fprintf because it is expensive. Doing it manually is about six times
// faster and makes the entire call of a logging function to stdout two times
// faster.
func addTimestamp(buffer *bytes.Buffer) {
	now := now()
	year, month, day := now.Date()
	hour, minute, second := now.Clock()

	var tmp = new([23]byte)
	writeInt(tmp, 4, 0, year)
	tmp[4] = '-'
	writeInt(tmp, 2, 5, int(month))
	tmp[7] = '-'
	writeInt(tmp, 2, 8, day)
	tmp[10] = ' '
	writeInt(tmp, 2, 11, hour)
	tmp[13] = ':'
	writeInt(tmp, 2, 14, minute)
	tmp[16] = ':'
	writeInt(tmp, 2, 17, second)
	tmp[19] = '.'
	writeInt(tmp, 3, 20, now.Nanosecond()/1000000)
	buffer.Write(tmp[:])
}

// A custom tiny helper functions to print integers efficiently
const digits = "0123456789"

func writeInt(tmp *[23]byte, intLength, position, integer int) {
	for i := intLength - 1; i >= 0; i-- {
		tmp[position+i] = digits[integer%10]
		integer /= 10
	}
}

var levelPrefixes = []string{
	" EMERGENCY ",
	" ALERT ",
	" CRITICAL ",
	" ERROR ",
	" WARNING ",
	" NOTICE ",
	" INFO ",
	" DEBUG ",
}

func addLevel(buffer *bytes.Buffer, level log.Level) {
	buffer.WriteString(levelPrefixes[level])
}

func addMessage(buffer *bytes.Buffer, args ...interface{}) {
	fmt.Fprintln(buffer, args...)
}

var defaultWriter = func(out io.Writer, logLine []byte, level log.Level) {
	out.Write(logLine)
}

// Creating a bytes.Buffer is expensive so we will re-use existing ones.
type buffer struct {
	bytes.Buffer
	next *buffer
}

// getBuffer returns a new, ready-to-use buffer.
func (logger *Logger) getBuffer() *buffer {
	logger.bufferListMutex.Lock()
	b := logger.bufferList
	if b != nil {
		logger.bufferList = b.next
	}
	logger.bufferListMutex.Unlock()
	if b == nil {
		b = new(buffer)
	} else {
		b.next = nil
		b.Reset()
	}
	return b
}

// putBuffer returns a buffer to the list.
func (logger *Logger) putBuffer(b *buffer) {
	if b.Len() >= 256 {
		// Let big buffers die a natural death.
		return
	}
	logger.bufferListMutex.Lock()
	b.next = logger.bufferList
	logger.bufferList = b
	logger.bufferListMutex.Unlock()
}

// Stubbed out for testing.
var now = time.Now
