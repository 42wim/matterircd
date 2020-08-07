package irckit

import (
	"github.com/sirupsen/logrus"
)

var (
	logger   *logrus.Entry
	LogLevel string
)

func SetLogger(l *logrus.Entry) {
	logger = l
}

func SetLogLevel(level string) {
	LogLevel = level
}

func IsDebugLevel() bool {
	return LogLevel == "debug"
}
