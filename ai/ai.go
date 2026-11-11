package ai

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
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

	roleUser          = "user"
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"

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

var logger *logrus.Entry

// SetLogger sets the logger for the ai package.
func SetLogger(l *logrus.Entry) {
	logger = l
}

func logWarnf(format string, args ...any) {
	if logger != nil {
		logger.Warnf(format, args...)
	}
}

const defaultMaxRetries = 3

func sleepWithContext(ctx context.Context, d time.Duration) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(d):
		return true, nil
	}
}

func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}

	header := resp.Header.Get("Retry-After")
	if header == "" {
		header = resp.Header.Get("X-RateLimit-Reset") //nolint:canonicalheader
	}

	if header != "" {
		seconds, sErr := strconv.Atoi(header)
		if sErr == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}

		resetTime, tErr := http.ParseTime(header)
		if tErr == nil {
			if wait := time.Until(resetTime); wait > 0 {
				return wait
			}
		}
	}

	return 0
}

func handleRetry(ctx context.Context, name string, err error, current, maxLimit int, resp *http.Response) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	if current >= maxLimit {
		return false, nil
	}

	sleepDuration := time.Duration(current+1) * time.Second

	if resp == nil {
		logWarnf("Network error on %s (resp is nil, err: %v), backing off: %s (attempt %d/%d)",
			name, err, sleepDuration, current+1, maxLimit)

		return sleepWithContext(ctx, sleepDuration)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := parseRetryAfter(resp)
		if wait <= 0 || wait > 30*time.Second {
			wait = sleepDuration
		}

		logWarnf("Rate limited (429) on %s, backing off: %s (attempt %d/%d)",
			name, wait, current+1, maxLimit)

		return sleepWithContext(ctx, wait)

	case resp.StatusCode >= http.StatusInternalServerError:
		logWarnf("Transient error %d on %s, backing off: %s (attempt %d/%d)",
			resp.StatusCode, name, sleepDuration, current+1, maxLimit)

		return sleepWithContext(ctx, sleepDuration)

	case resp.StatusCode >= 300 && resp.StatusCode < 500:
		// Client errors (4xx) usually shouldn't be retried unless it's a 429
		return false, nil

	default:
		return sleepWithContext(ctx, sleepDuration)
	}
}

func doPostWithRetry(ctx context.Context, client *http.Client, name, url string, headers map[string]string, body []byte) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}

	retryCount := 0

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}

		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		shouldRetry, hErr := handleRetry(ctx, name, err, retryCount, defaultMaxRetries, resp)
		if hErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}

			return nil, hErr
		}

		if shouldRetry {
			if resp != nil {
				_ = resp.Body.Close()
			}

			retryCount++

			continue
		}

		if err != nil {
			return nil, err
		}

		return resp, nil
	}
}
