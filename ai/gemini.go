package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type GeminiClient struct {
	apiKey      string
	baseURL     string
	project     string
	location    string
	model       string
	tokenSource oauth2.TokenSource
	httpClient  *http.Client
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

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
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
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
	Error         *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func parseGeminiUsage(m *geminiUsageMetadata) Usage {
	if m == nil {
		return Usage{}
	}

	res := Usage{
		PromptTokens:     m.PromptTokenCount,
		CompletionTokens: m.CandidatesTokenCount,
		TotalTokens:      m.TotalTokenCount,
		ReasoningTokens:  m.ThoughtsTokenCount,
	}

	if res.TotalTokens == 0 && (res.PromptTokens > 0 || res.CompletionTokens > 0) {
		res.TotalTokens = res.PromptTokens + res.CompletionTokens
	}

	return res
}

// NewGeminiClient creates a new Gemini AI client authenticated via service account credentials.
func NewGeminiClient(ctx context.Context, saFile, project, location, model string) (*GeminiClient, error) {
	data, err := os.ReadFile(saFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read service account file: %w", err)
	}

	if project == "" {
		var sa struct {
			ProjectID string `json:"project_id"`
		}

		err = json.NewDecoder(bytes.NewReader(data)).Decode(&sa)
		if err == nil && sa.ProjectID != "" {
			project = sa.ProjectID
		}
	}

	if project == "" {
		return nil, errors.New("gcp project is not specified and could not be found in service account file")
	}

	jwtCfg, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("failed to parse service account credentials: %w", err)
	}

	if model == "" {
		model = DefaultGeminiModel
	}

	return &GeminiClient{
		project:     project,
		location:    location,
		model:       model,
		tokenSource: jwtCfg.TokenSource(ctx),
		httpClient:  &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// NewGeminiAPIKeyClient creates a new Gemini AI client targeting Google AI Studio authenticated via API key.
func NewGeminiAPIKeyClient(apiKey, baseURL, model string) (*GeminiClient, error) {
	if apiKey == "" {
		return nil, errors.New("gemini API key is required")
	}

	if baseURL == "" {
		baseURL = DefaultGeminiBaseURL
	}

	if model == "" {
		model = DefaultGeminiModel
	}

	return &GeminiClient{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		model:      model,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Project returns the resolved GCP project ID.
func (c *GeminiClient) Project() string {
	return c.project
}

// BaseURL returns the configured base URL.
func (c *GeminiClient) BaseURL() string {
	if c.baseURL != "" {
		return c.baseURL
	}

	return DefaultGeminiBaseURL
}

//nolint:funlen,gocyclo
func (c *GeminiClient) Summarize(ctx context.Context, prompt string, thinking string) (*SummaryResult, error) {
	var (
		url        string
		authHeader = "Authorization"
		authValue  string
	)

	switch {
	case c.apiKey != "":
		baseURL := c.baseURL
		if baseURL == "" {
			baseURL = DefaultGeminiBaseURL
		}

		url = fmt.Sprintf("%s/models/%s:generateContent", strings.TrimRight(baseURL, "/"), c.model)
		authHeader = "x-goog-api-key"
		authValue = c.apiKey
	case c.tokenSource != nil:
		token, err := c.tokenSource.Token()
		if err != nil {
			return nil, fmt.Errorf("failed to get oauth2 token: %w", err)
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

		url = fmt.Sprintf(
			"https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
			endpointHost, c.project, c.location, c.model,
		)
		authValue = "Bearer " + token.AccessToken
	default:
		return nil, errors.New("gemini client has neither API key nor service account credentials configured")
	}

	reqBody := generateRequest{
		Contents: []content{
			{
				Role:  roleUser,
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
		return nil, err
	}

	headers := map[string]string{
		authHeader:        authValue,
		headerContentType: contentTypeJSON,
	}

	resp, err := doPostWithRetry(ctx, c.httpClient, "gemini", url, headers, rawJSON)
	if err != nil {
		return nil, fmt.Errorf("gemini API request failed: %w", err)
	}
	defer resp.Body.Close()

	var genResp generateResponse

	err = json.NewDecoder(resp.Body).Decode(&genResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if genResp.Error != nil {
		return nil, fmt.Errorf("gemini error: %s", genResp.Error.Message)
	}

	if len(genResp.Candidates) > 0 {
		var sb strings.Builder

		for _, p := range genResp.Candidates[0].Content.Parts {
			if !p.Thought && p.Text != "" {
				sb.WriteString(p.Text)
			}
		}

		resultText := sb.String()
		if resultText == "" && len(genResp.Candidates[0].Content.Parts) > 0 {
			resultText = genResp.Candidates[0].Content.Parts[len(genResp.Candidates[0].Content.Parts)-1].Text
		}

		if resultText != "" {
			return &SummaryResult{Content: resultText, Usage: parseGeminiUsage(genResp.UsageMetadata)}, nil
		}
	}

	return nil, fmt.Errorf("no summary generated")
}
