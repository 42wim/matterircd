package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	openRouterReferer = "https://github.com/42wim/matterircd"
	openRouterTitle   = "matterircd"
)

type OpenRouterClient struct {
	token      string
	baseURL    string
	model      string
	httpClient *http.Client
}

type openRouterReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Exclude bool   `json:"exclude,omitempty"`
}

type openRouterRequest struct {
	Model     string               `json:"model"`
	Messages  []chatMessage        `json:"messages"`
	Reasoning *openRouterReasoning `json:"reasoning,omitempty"`
}

func resolveOpenRouterReasoning(mode string) *openRouterReasoning {
	m := strings.ToLower(strings.TrimSpace(mode))

	switch m {
	case thinkingOff, thinkingNone:
		return &openRouterReasoning{Effort: thinkingNone, Exclude: true}
	case thinkingLow, thinkingMedium, thinkingHigh:
		return &openRouterReasoning{Effort: m, Exclude: true}
	case thinkingMed:
		return &openRouterReasoning{Effort: thinkingMedium, Exclude: true}
	default:
		// Omit for "" or "auto" so non-reasoning and mandatory-reasoning
		// models use their native defaults without rejecting the parameter.
		return nil
	}
}

// NewOpenRouterClient creates a client targeting the OpenRouter Chat Completions API.
func NewOpenRouterClient(token, baseURL, model string) (*OpenRouterClient, error) {
	if token == "" {
		return nil, errors.New("openrouter token is required")
	}

	if baseURL == "" {
		baseURL = DefaultOpenRouterBaseURL
	}

	baseURL = strings.TrimRight(baseURL, "/")

	if model == "" {
		model = DefaultOpenRouterModel
	}

	return &OpenRouterClient{
		token:      token,
		baseURL:    baseURL,
		model:      model,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// BaseURL returns the configured base URL.
func (c *OpenRouterClient) BaseURL() string {
	return c.baseURL
}

//nolint:funlen
func (c *OpenRouterClient) Summarize(ctx context.Context, prompt string, thinking string) (*SummaryResult, error) {
	reqBody := openRouterRequest{
		Model: c.model,
		Messages: []chatMessage{
			{
				Role:    roleUser,
				Content: prompt,
			},
		},
		Reasoning: resolveOpenRouterReasoning(thinking),
	}

	rawJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := c.baseURL + "/chat/completions"

	headers := map[string]string{
		"Authorization":   "Bearer " + c.token,
		headerContentType: contentTypeJSON,
		"HTTP-Referer":    openRouterReferer,
		"X-Title":         openRouterTitle,
	}

	resp, err := doPostWithRetry(ctx, c.httpClient, AIOpenRouterProvider, url, headers, rawJSON)
	if err != nil {
		return nil, fmt.Errorf("OpenRouter API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}

		var errMsg string

		err = json.NewDecoder(resp.Body).Decode(&errResp)
		if err == nil && errResp.Error != nil && errResp.Error.Message != "" {
			errMsg = strings.Join(strings.Fields(errResp.Error.Message), " ")
		}

		if errMsg != "" {
			return nil, fmt.Errorf("OpenRouter API returned status %s: %s", resp.Status, errMsg)
		}

		return nil, fmt.Errorf("OpenRouter API returned status %s", resp.Status)
	}

	var chatResp chatResponse

	err = json.NewDecoder(resp.Body).Decode(&chatResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if chatResp.Error != nil {
		errMsg := strings.Join(strings.Fields(chatResp.Error.Message), " ")

		return nil, fmt.Errorf("OpenRouter error: %s", errMsg)
	}

	if len(chatResp.Choices) > 0 {
		content := strings.TrimSpace(chatResp.Choices[0].Message.Content)
		if content != "" {
			return &SummaryResult{
				Content: content,
				Model:   strings.TrimSpace(chatResp.Model),
				Usage:   parseChatUsage(chatResp.Usage),
			}, nil
		}
	}

	return nil, errors.New("no summary generated")
}
