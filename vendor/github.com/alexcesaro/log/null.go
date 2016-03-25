package log

// NullLogger is a no-op instance of the Logger interface.
var NullLogger = nullLogger{}

// nullLogger implements a no-op type of the Logger interface.
type nullLogger struct{}

func (l nullLogger) Emergency(args ...interface{})                 {}
func (l nullLogger) Emergencyf(format string, args ...interface{}) {}
func (l nullLogger) Alert(args ...interface{})                     {}
func (l nullLogger) Alertf(format string, args ...interface{})     {}
func (l nullLogger) Critical(args ...interface{})                  {}
func (l nullLogger) Criticalf(format string, args ...interface{})  {}
func (l nullLogger) Error(args ...interface{})                     {}
func (l nullLogger) Errorf(format string, args ...interface{})     {}
func (l nullLogger) Warning(args ...interface{})                   {}
func (l nullLogger) Warningf(format string, args ...interface{})   {}
func (l nullLogger) Notice(args ...interface{})                    {}
func (l nullLogger) Noticef(format string, args ...interface{})    {}
func (l nullLogger) Info(args ...interface{})                      {}
func (l nullLogger) Infof(format string, args ...interface{})      {}
func (l nullLogger) Debug(args ...interface{})                     {}
func (l nullLogger) Debugf(format string, args ...interface{})     {}

func (l nullLogger) Log(level Level, args ...interface{})                 {}
func (l nullLogger) Logf(level Level, format string, args ...interface{}) {}

func (l nullLogger) LogEmergency() bool        { return false }
func (l nullLogger) LogAlert() bool            { return false }
func (l nullLogger) LogCritical() bool         { return false }
func (l nullLogger) LogError() bool            { return false }
func (l nullLogger) LogWarning() bool          { return false }
func (l nullLogger) LogNotice() bool           { return false }
func (l nullLogger) LogInfo() bool             { return false }
func (l nullLogger) LogDebug() bool            { return false }
func (l nullLogger) LogLevel(level Level) bool { return false }

func (l nullLogger) Close() error { return nil }
