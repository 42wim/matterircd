//go:build noai

package main

import (
	"github.com/42wim/matterircd/config"
	"github.com/sirupsen/logrus"
)

const aiEnabled = false

func setAILogger(*logrus.Entry) {}

func initAI(l *logrus.Entry, aiCfg config.AIConfig) {
	if aiCfg.Enabled {
		l.Warn("AI summarization is enabled in config, but NOT ENABLED in this build - rebuild without `-tags=noai`")

		return
	}

	l.Info("AI summarization NOT ENABLED in this build (built with `-tags=noai`)")
}
