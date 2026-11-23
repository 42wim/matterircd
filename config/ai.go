//go:build !noai

package config

import (
	"strings"

	"github.com/42wim/matterircd/ai"
)

// GetBaseURL resolves the endpoint base URL for the requested provider, ensuring
// credentials and requests are not dispatched to unrelated endpoints.
//
//nolint:funlen,gocyclo,gocognit
func (c *AIConfig) GetBaseURL(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))

	if c.URLs != nil {
		if u, ok := c.URLs[p]; ok && u != "" {
			return strings.TrimRight(u, "/")
		}

		if p == ai.AIClaudeProvider && c.URLs[ai.AIAnthropicProvider] != "" {
			return strings.TrimRight(c.URLs[ai.AIAnthropicProvider], "/")
		} else if p == ai.AIAnthropicProvider && c.URLs[ai.AIClaudeProvider] != "" {
			return strings.TrimRight(c.URLs[ai.AIClaudeProvider], "/")
		}
	}

	cfgP := strings.ToLower(strings.TrimSpace(c.Provider))

	switch p {
	case ai.AIClaudeProvider, ai.AIAnthropicProvider:
		if (cfgP == ai.AIClaudeProvider || cfgP == ai.AIAnthropicProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultClaudeBaseURL
	case ai.AIDeepSeekProvider:
		if cfgP == ai.AIDeepSeekProvider && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultDeepSeekBaseURL
	case ai.AIGeminiProvider, ai.AIGoogleProvider:
		if (cfgP == ai.AIGeminiProvider || cfgP == ai.AIGoogleProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultGeminiBaseURL
	case ai.AIHetznerProvider:
		if cfgP == ai.AIHetznerProvider && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultHetznerBaseURL
	case ai.AIMuseProvider, ai.AIMetaProvider:
		if (cfgP == ai.AIMuseProvider || cfgP == ai.AIMetaProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultMuseBaseURL
	case ai.AIOpenAIProvider:
		if cfgP == ai.AIOpenAIProvider && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultOpenAIBaseURL
	case ai.AIOpenRouterProvider:
		if cfgP == ai.AIOpenRouterProvider && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultOpenRouterBaseURL
	case ai.AIOllamaProvider, ai.AILocalProvider, ai.AIvLLMProvider:
		if (cfgP == p || cfgP == ai.AILocalProvider || cfgP == ai.AIOllamaProvider || cfgP == ai.AIvLLMProvider || cfgP == "" || cfgP == ai.AIGeminiProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ""
	case ai.AIXAIProvider, ai.AIGrokProvider:
		if (cfgP == ai.AIXAIProvider || cfgP == ai.AIGrokProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultXAIBaseURL
	default:
		return ""
	}
}

// GetToken resolves the token for the requested provider, ensuring credentials
// do not cross provider or endpoint boundaries while preserving the legacy Copilot token fallback.
//
//nolint:funlen,gocyclo,gocognit
func (c *AIConfig) GetToken(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))

	switch p {
	case ai.AIClaudeProvider, ai.AIAnthropicProvider:
		if c.ClaudeToken != "" {
			return c.ClaudeToken
		}

		if strings.EqualFold(c.Provider, ai.AIClaudeProvider) || strings.EqualFold(c.Provider, ai.AIAnthropicProvider) {
			return c.Token
		}
	case ai.AICopilotProvider, ai.AIGitHubProvider:
		if c.CopilotToken != "" {
			return c.CopilotToken
		}

		cfgP := strings.ToLower(c.Provider)
		if cfgP == "" || (cfgP == ai.AIGeminiProvider && c.ServiceAccountFile != "") || cfgP == ai.AICopilotProvider || cfgP == ai.AIGitHubProvider {
			return c.Token
		}
	case ai.AIDeepSeekProvider:
		if c.DeepSeekToken != "" {
			return c.DeepSeekToken
		}

		if strings.EqualFold(c.Provider, ai.AIDeepSeekProvider) {
			return c.Token
		}
	case ai.AIGeminiProvider, ai.AIGoogleProvider:
		if c.GeminiToken != "" {
			return c.GeminiToken
		}

		if strings.EqualFold(c.Provider, ai.AIGeminiProvider) || strings.EqualFold(c.Provider, ai.AIGoogleProvider) {
			return c.Token
		}
	case ai.AIHetznerProvider:
		if c.HetznerToken != "" {
			return c.HetznerToken
		}

		if strings.EqualFold(c.Provider, ai.AIHetznerProvider) {
			return c.Token
		}
	case ai.AIMuseProvider, ai.AIMetaProvider:
		if c.MuseToken != "" {
			return c.MuseToken
		}

		if strings.EqualFold(c.Provider, ai.AIMuseProvider) || strings.EqualFold(c.Provider, ai.AIMetaProvider) {
			return c.Token
		}
	case ai.AIOpenAIProvider:
		if c.OpenAIToken != "" {
			return c.OpenAIToken
		}

		if strings.EqualFold(c.Provider, ai.AIOpenAIProvider) {
			return c.Token
		}
	case ai.AIOpenRouterProvider:
		if c.OpenRouterToken != "" {
			return c.OpenRouterToken
		}

		if strings.EqualFold(c.Provider, ai.AIOpenRouterProvider) {
			return c.Token
		}
	case ai.AIOllamaProvider, ai.AILocalProvider, ai.AIvLLMProvider:
		if strings.EqualFold(c.Provider, p) {
			return c.Token
		}
	case ai.AIXAIProvider, ai.AIGrokProvider:
		if c.XAIToken != "" {
			return c.XAIToken
		}

		if strings.EqualFold(c.Provider, ai.AIXAIProvider) || strings.EqualFold(c.Provider, ai.AIGrokProvider) {
			return c.Token
		}
	}

	return ""
}
