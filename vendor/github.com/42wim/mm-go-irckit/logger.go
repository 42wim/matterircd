package irckit

import (
	"github.com/Sirupsen/logrus"
)

var logger *logrus.Entry
var LogLevel string

func SetLogger(l *logrus.Entry) {
	logger = l
}

func SetLogLevel(level string) {
	LogLevel = level
}

func IsDebugLevel() bool {
	if LogLevel == "debug" {
		return true
	}
	return false
}
