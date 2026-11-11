package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type CopilotClient struct {
	token      string
	model      string
	httpClient *http.Client
}

type copilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

type copilotRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

// NewCopilotClient creates a client targeting the native GitHub Copilot API.
func NewCopilotClient(token, model string) (*CopilotClient, error) {
	if token == "" {
		return nil, fmt.Errorf("gitHub token is required for Copilot summarizer")
	}

	if model == "" {
		model = DefaultCopilotModel
	}

	return &CopilotClient{
		token:      token,
		model:      model,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

//nolint:funlen
func (c *CopilotClient) Summarize(ctx context.Context, prompt string, _ string) (*SummaryResult, error) {
	sessionToken, err := c.getGitHubSessionToken(ctx)
	if err != nil {
		return nil, err
	}

	reqBody := copilotRequest{
		Model: c.model,
		Messages: []chatMessage{
			{
				Role:    roleUser,
				Content: prompt,
			},
		},
		Temperature: 0.2,
	}

	rawJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := "https://api.githubcopilot.com/chat/completions"

	headers := map[string]string{
		"Authorization":          "Bearer " + sessionToken,
		headerContentType:        contentTypeJSON,
		"Editor-Version":         "vscode/1.98.0",
		"Editor-Plugin-Version":  "copilot-chat/0.24.0",
		"Copilot-Integration-Id": "vscode-chat",
	}

	resp, err := doPostWithRetry(ctx, c.httpClient, "copilot", url, headers, rawJSON)
	if err != nil {
		return nil, fmt.Errorf("copilot API request failed: %w", err)
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
			return nil, fmt.Errorf("copilot API returned status %s: %s", resp.Status, errMsg)
		}

		return nil, fmt.Errorf("copilot API returned status %s", resp.Status)
	}

	var chatResp chatResponse

	err = json.NewDecoder(resp.Body).Decode(&chatResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if chatResp.Error != nil {
		errMsg := strings.Join(strings.Fields(chatResp.Error.Message), " ")

		return nil, fmt.Errorf("copilot error: %s", errMsg)
	}

	if len(chatResp.Choices) > 0 {
		content := strings.TrimSpace(chatResp.Choices[0].Message.Content)
		if content != "" {
			return &SummaryResult{Content: content, Usage: parseChatUsage(chatResp.Usage)}, nil
		}
	}

	return nil, fmt.Errorf("no summary generated")
}

func (c *CopilotClient) getGitHubSessionToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/copilot_internal/v2/token", nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.98.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.24.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot auth request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("copilot auth returned status %s", resp.Status)
	}

	var tokenResp copilotTokenResponse

	err = json.NewDecoder(resp.Body).Decode(&tokenResp)
	if err != nil {
		return "", fmt.Errorf("failed to decode Copilot token: %w", err)
	}

	return tokenResp.Token, nil
}
