package irckit

import (
	"github.com/Sirupsen/logrus"
)

var logger *logrus.Entry

func SetLogger(l *logrus.Entry) {
	logger = l
}

func IsDebugLevel() bool {
	if logger.Level == logrus.DebugLevel {
		return true
	}
	return false
}
