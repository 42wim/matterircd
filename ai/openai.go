package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type OpenAIClient struct {
	token      string
	baseURL    string
	model      string
	httpClient *http.Client
}

type openAIRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
}

func isOpenAIReasoningModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))

	return strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") || strings.HasPrefix(m, "gpt-5") || strings.HasPrefix(m, "deepseek-reasoner") || strings.HasPrefix(m, "deepseek-r1") || strings.HasPrefix(m, "grok-reasoner")
}

// NewOpenAIClient creates a client targeting standard OpenAI or compatible REST endpoints.
func NewOpenAIClient(token, baseURL, model string) (*OpenAIClient, error) {
	if baseURL == "" {
		return nil, errors.New("base url is required")
	}

	baseURL = strings.TrimRight(baseURL, "/")

	if model == "" {
		model = DefaultOpenAIModel
	}

	return &OpenAIClient{
		token:      token,
		baseURL:    baseURL,
		model:      model,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// BaseURL returns the configured base URL.
func (c *OpenAIClient) BaseURL() string {
	return c.baseURL
}

//nolint:funlen
func (c *OpenAIClient) Summarize(ctx context.Context, prompt string, _ string) (string, error) {
	reqBody := openAIRequest{
		Model: c.model,
		Messages: []chatMessage{
			{
				Role:    roleUser,
				Content: prompt,
			},
		},
	}

	if !isOpenAIReasoningModel(c.model) {
		temp := 0.2
		reqBody.Temperature = &temp
	}

	rawJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	url := c.baseURL + "/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("OpenAI API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}

		errMsg := string(body)

		err = json.Unmarshal(body, &errResp)
		if err == nil && errResp.Error != nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		}

		errMsg = strings.Join(strings.Fields(errMsg), " ")

		return "", fmt.Errorf("OpenAI API returned status %s: %s", resp.Status, errMsg)
	}

	var chatResp chatResponse

	err = json.Unmarshal(body, &chatResp)
	if err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if chatResp.Error != nil {
		errMsg := strings.Join(strings.Fields(chatResp.Error.Message), " ")

		return "", fmt.Errorf("OpenAI error: %s", errMsg)
	}

	if len(chatResp.Choices) > 0 {
		content := strings.TrimSpace(chatResp.Choices[0].Message.Content)
		if content != "" {
			return content, nil
		}
	}

	return "", errors.New("no summary generated")
}
