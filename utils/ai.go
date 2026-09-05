package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Summarizer interface {
	Summarize(ctx context.Context, prompt string, thinking string) (string, error)
}

type Client struct {
	project     string
	location    string
	model       string
	tokenSource oauth2.TokenSource
	httpClient  *http.Client
}

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
	Model       string           `json:"model"`
	Messages    []copilotMessage `json:"messages"`
	Temperature float64          `json:"temperature"`
}

type copilotMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type copilotResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type generateRequest struct {
	Contents         []content         `json:"contents"`
	GenerationConfig *generationConfig `json:"generationConfig,omitempty"`
}

type generationConfig struct {
	ThinkingConfig *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	ThinkingBudget *int `json:"thinkingBudget,omitempty"`
}

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type generateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought,omitempty"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NewCopilotClient creates a client targeting the native GitHub Copilot API.
func NewCopilotClient(token, model string) (*CopilotClient, error) {
	if token == "" {
		return nil, fmt.Errorf("github token is required for copilot summarizer")
	}

	if model == "" {
		model = "gpt-4o"
	}

	return &CopilotClient{
		token:      token,
		model:      model,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// NewGeminiAIClient creates a new Gemini AI client authenticated via service account credentials.
func NewGeminiAIClient(ctx context.Context, saFile, project, location, model string) (*Client, error) {
	data, err := os.ReadFile(saFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read service account file: %w", err)
	}

	jwtCfg, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("failed to parse service account credentials: %w", err)
	}

	if model == "" {
		model = "gemini-3.7-flash"
	}

	return &Client{
		project:     project,
		location:    location,
		model:       model,
		tokenSource: jwtCfg.TokenSource(ctx),
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

//nolint:funlen,gocyclo
func (c *Client) Summarize(ctx context.Context, prompt string, thinking string) (string, error) {
	token, err := c.tokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("failed to get oauth2 token: %w", err)
	}

	var endpointHost string

	switch c.location {
	case "", "global", "us":
		endpointHost = "aiplatform.googleapis.com"
	case "eu":
		endpointHost = "eu-aiplatform.googleapis.com"
	default:
		endpointHost = fmt.Sprintf("%s-aiplatform.googleapis.com", c.location)
	}

	url := fmt.Sprintf(
		"https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		endpointHost, c.project, c.location, c.model,
	)

	reqBody := generateRequest{
		Contents: []content{
			{
				Role:  "user",
				Parts: []part{{Text: prompt}},
			},
		},
	}

	if budget := resolveThinkingBudget(thinking); budget != nil {
		reqBody.GenerationConfig = &generationConfig{
			ThinkingConfig: &thinkingConfig{
				ThinkingBudget: budget,
			},
		}
	}

	rawJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini api request failed: %w", err)
	}
	defer resp.Body.Close()

	var genResp generateResponse

	err = json.NewDecoder(resp.Body).Decode(&genResp)
	if err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if genResp.Error != nil {
		return "", fmt.Errorf("gemini error: %s", genResp.Error.Message)
	}

	if len(genResp.Candidates) > 0 {
		var sb strings.Builder

		for _, p := range genResp.Candidates[0].Content.Parts {
			if !p.Thought && p.Text != "" {
				sb.WriteString(p.Text)
			}
		}

		if sb.Len() > 0 {
			return sb.String(), nil
		}

		if len(genResp.Candidates[0].Content.Parts) > 0 {
			return genResp.Candidates[0].Content.Parts[len(genResp.Candidates[0].Content.Parts)-1].Text, nil
		}
	}

	return "", fmt.Errorf("no summary generated")
}

func (c *CopilotClient) Summarize(ctx context.Context, prompt string, _ string) (string, error) {
	sessionToken, err := c.getGitHubSessionToken(ctx)
	if err != nil {
		return "", err
	}

	reqBody := copilotRequest{
		Model: c.model,
		Messages: []copilotMessage{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		Temperature: 0.2,
	}

	rawJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	url := "https://api.githubcopilot.com/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+sessionToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.98.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.24.0")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot api request failed: %w", err)
	}
	defer resp.Body.Close()

	var chatResp copilotResponse

	err = json.NewDecoder(resp.Body).Decode(&chatResp)
	if err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if chatResp.Error != nil {
		return "", fmt.Errorf("copilot error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) > 0 {
		return strings.TrimSpace(chatResp.Choices[0].Message.Content), nil
	}

	return "", fmt.Errorf("no summary generated")
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
		return "", fmt.Errorf("failed to decode copilot token: %w", err)
	}

	return tokenResp.Token, nil
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
