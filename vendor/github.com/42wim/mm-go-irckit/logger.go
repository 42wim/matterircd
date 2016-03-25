package irckit

import (
	"github.com/alexcesaro/log"
)

var logger log.Logger = log.NullLogger

func SetLogger(l log.Logger) {
	logger = l
}
