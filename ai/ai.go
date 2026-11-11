package ai

import (
	"context"
	"strings"
)

type Summarizer interface {
	Summarize(ctx context.Context, prompt string, thinking string) (*SummaryResult, error)
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int
}

type SummaryResult struct {
	Content string
	Usage   Usage
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatUsage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func parseChatUsage(u *chatUsage) Usage {
	if u == nil {
		return Usage{}
	}

	res := Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}

	if u.CompletionTokensDetails != nil {
		res.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}

	if res.TotalTokens == 0 && (res.PromptTokens > 0 || res.CompletionTokens > 0) {
		res.TotalTokens = res.PromptTokens + res.CompletionTokens
	}

	return res
}

const (
	AIAnthropicProvider = "anthropic"
	AIClaudeProvider    = "claude"
	AICopilotProvider   = "copilot"
	AIDeepSeekProvider  = "deepseek"
	AIGeminiProvider    = "gemini"
	AIGitHubProvider    = "github"
	AIGoogleProvider    = "google"
	AIGrokProvider      = "grok"
	AIHetznerProvider   = "hetzner"
	AILocalProvider     = "local"
	AIMetaProvider      = "meta"
	AIMuseProvider      = "muse"
	AIOllamaProvider    = "ollama"
	AIOpenAIProvider    = "openai"
	AIvLLMProvider      = "vllm"
	AIXAIProvider       = "xai"

	roleUser = "user"

	DefaultClaudeModel   = "claude-sonnet-5"
	DefaultCopilotModel  = "gpt-4o"
	DefaultDeepSeekModel = "deepseek-chat"
	DefaultGeminiModel   = "gemini-3.8-flash"
	DefaultHetznerModel  = "meta-llama/Meta-Llama-3.1-70B-Instruct"
	DefaultLocalModel    = "llama3.2"
	DefaultMuseModel     = "muse-spark-1.3"
	DefaultOpenAIModel   = "gpt-4o"
	DefaultXAIModel      = "grok-2"

	DefaultClaudeBaseURL   = "https://api.anthropic.com/v1"
	DefaultDeepSeekBaseURL = "https://api.deepseek.com"
	DefaultGeminiBaseURL   = "https://generativelanguage.googleapis.com/v1beta"
	DefaultHetznerBaseURL  = "https://inference.hetzner.com/api/v1"
	DefaultMuseBaseURL     = "https://api.meta.ai/v1"
	DefaultOpenAIBaseURL   = "https://api.openai.com/v1"
	DefaultXAIBaseURL      = "https://api.x.ai/v1"

	AnthropicAPIVersion = "2023-06-01"
)

// GetModel resolves the AI model to use based on provider, fallback model, and the models map.
//
//nolint:gocyclo
func GetModel(provider, fallbackModel string, models map[string]string) string {
	p := strings.ToLower(strings.TrimSpace(provider))

	if models != nil {
		if m, ok := models[p]; ok && m != "" {
			return m
		}

		if p == AIClaudeProvider && models[AIAnthropicProvider] != "" {
			return models[AIAnthropicProvider]
		} else if p == AIAnthropicProvider && models[AIClaudeProvider] != "" {
			return models[AIClaudeProvider]
		}
	}

	if fallbackModel != "" {
		return fallbackModel
	}

	switch p {
	case AIClaudeProvider, AIAnthropicProvider:
		return DefaultClaudeModel
	case AICopilotProvider, AIGitHubProvider:
		return DefaultCopilotModel
	case AIDeepSeekProvider:
		return DefaultDeepSeekModel
	case AIHetznerProvider:
		return DefaultHetznerModel
	case AIMuseProvider, AIMetaProvider:
		return DefaultMuseModel
	case AIOpenAIProvider:
		return DefaultOpenAIModel
	case AIOllamaProvider, AILocalProvider, AIvLLMProvider:
		return DefaultLocalModel
	case AIXAIProvider, AIGrokProvider:
		return DefaultXAIModel
	default:
		return DefaultGeminiModel
	}
}

func resolveThinkingBudget(mode string) *int {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off", "none":
		b := 0

		return &b
	case "low":
		b := 1024

		return &b
	case "medium", "med":
		b := 4096

		return &b
	case "high":
		b := 16384

		return &b
	default:
		return nil
	}
}
