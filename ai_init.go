//go:build !noai

package main

import (
	"context"
	"strings"
	"time"

	"github.com/42wim/matterircd/ai"
	"github.com/42wim/matterircd/config"
	"github.com/sirupsen/logrus"
)

const aiFeature = "ai"

func setAILogger(l *logrus.Entry) {
	ai.SetLogger(l)
}

//nolint:funlen,gocyclo
func initAI(l *logrus.Entry, aiCfg config.AIConfig) {
	if !aiCfg.Enabled {
		l.Debug("AI summarization is disabled")

		return
	}

	provider := aiCfg.Provider
	if provider == "" {
		provider = ai.AIGeminiProvider
	}

	model := ai.GetModel(provider, aiCfg.Model, aiCfg.Models)
	token := aiCfg.GetToken(provider)

	switch strings.ToLower(provider) {
	case ai.AICopilotProvider, ai.AIGitHubProvider:
		if token == "" {
			l.Warn("AI summarization enabled, but token is missing")

			return
		}

		_, err := ai.NewCopilotClient(token, model)
		if err != nil {
			l.Errorf("AI summarization setup error: %v", err)

			return
		}

		l.Infof("AI summarization enabled (default provider: %s, model: %s)", provider, model)
	case ai.AIOpenRouterProvider:
		if token == "" {
			l.Warn("AI summarization enabled, but token is missing")

			return
		}

		baseURL := aiCfg.GetBaseURL(provider)

		client, err := ai.NewOpenRouterClient(token, baseURL, model)
		if err != nil {
			l.Errorf("AI summarization setup error: %v", err)

			return
		}

		l.Infof("AI summarization enabled (default provider: %s, model: %s, url: %s)", provider, model, client.BaseURL())
	case ai.AIOpenAIProvider, ai.AIHetznerProvider, ai.AIOllamaProvider, ai.AILocalProvider, ai.AIvLLMProvider:
		baseURL := aiCfg.GetBaseURL(provider)
		if baseURL == "" {
			l.Warnf("AI summarization enabled for %s, but url is missing", provider)

			return
		}

		if token == "" && (strings.Contains(baseURL, "api.openai.com") || strings.Contains(baseURL, "inference.hetzner.com")) {
			l.Warn("AI summarization enabled, but token is missing")

			return
		}

		client, err := ai.NewOpenAIClient(token, baseURL, model)
		if err != nil {
			l.Errorf("AI summarization setup error: %v", err)

			return
		}

		l.Infof("AI summarization enabled (default provider: %s, model: %s, url: %s)", provider, model, client.BaseURL())
	default:
		if aiCfg.ServiceAccountFile == "" && token == "" {
			l.Warn("AI summarization enabled, but service_account_file or token is missing")

			return
		}

		if aiCfg.ServiceAccountFile != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			client, err := ai.NewGeminiClient(ctx, aiCfg.ServiceAccountFile, aiCfg.Project, aiCfg.Location, model)
			if err != nil {
				l.Errorf("AI summarization setup error: %v", err)

				return
			}

			l.Infof(
				"AI summarization enabled (default provider: %s, model: %s, project: %s, region: %s)",
				provider, model, client.Project(), aiCfg.Location,
			)

			return
		}

		baseURL := aiCfg.GetBaseURL(provider)

		client, err := ai.NewGeminiAPIKeyClient(token, baseURL, model)
		if err != nil {
			l.Errorf("AI summarization setup error: %v", err)

			return
		}

		l.Infof(
			"AI summarization enabled (default provider: %s, model: %s, url: %s)",
			provider, model, client.BaseURL(),
		)
	}
}
