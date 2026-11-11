package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ClaudeClient struct {
	token      string
	baseURL    string
	model      string
	httpClient *http.Client
}

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []chatMessage   `json:"messages"`
	Thinking  *claudeThinking `json:"thinking,omitempty"`
}

type claudeThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type claudeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage *claudeUsage `json:"usage,omitempty"`
	Error *struct {
		Type    string `json:"type,omitempty"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func parseClaudeUsage(u *claudeUsage) Usage {
	if u == nil {
		return Usage{}
	}

	return Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

// NewClaudeClient creates a client targeting Anthropic Claude Messages REST API.
func NewClaudeClient(token, baseURL, model string) (*ClaudeClient, error) {
	if token == "" {
		return nil, errors.New("anthropic token is required")
	}

	if baseURL == "" {
		baseURL = DefaultClaudeBaseURL
	}

	baseURL = strings.TrimRight(baseURL, "/")

	if model == "" {
		model = DefaultClaudeModel
	}

	return &ClaudeClient{
		token:      token,
		baseURL:    baseURL,
		model:      model,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// BaseURL returns the configured base URL.
func (c *ClaudeClient) BaseURL() string {
	return c.baseURL
}

//nolint:funlen,gocyclo
func (c *ClaudeClient) Summarize(ctx context.Context, prompt string, thinking string) (*SummaryResult, error) {
	maxTokens := 4096

	reqBody := claudeRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		Messages: []chatMessage{
			{
				Role:    roleUser,
				Content: prompt,
			},
		},
	}

	budget := resolveThinkingBudget(thinking)
	if budget != nil && *budget >= 1024 {
		reqBody.Thinking = &claudeThinking{
			Type:         "enabled",
			BudgetTokens: *budget,
		}

		if *budget >= maxTokens {
			reqBody.MaxTokens = *budget + 2048
		}
	}

	rawJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := c.baseURL + "/messages"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-Api-Key", c.token)
	req.Header.Set("Anthropic-Version", AnthropicAPIVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic API request failed: %w", err)
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
			return nil, fmt.Errorf("anthropic API returned status %s: %s", resp.Status, errMsg)
		}

		return nil, fmt.Errorf("anthropic API returned status %s", resp.Status)
	}

	var chatResp claudeResponse

	err = json.NewDecoder(resp.Body).Decode(&chatResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if chatResp.Error != nil {
		errMsg := strings.Join(strings.Fields(chatResp.Error.Message), " ")

		return nil, fmt.Errorf("anthropic error: %s", errMsg)
	}

	var sb strings.Builder

	for _, block := range chatResp.Content {
		if block.Type == "text" && block.Text != "" {
			sb.WriteString(block.Text)
		}
	}

	if sb.Len() > 0 {
		return &SummaryResult{Content: sb.String(), Usage: parseClaudeUsage(chatResp.Usage)}, nil
	}

	return nil, errors.New("no summary generated")
}
