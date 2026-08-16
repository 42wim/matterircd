package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Client struct {
	project     string
	location    string
	model       string
	tokenSource oauth2.TokenSource
	httpClient  *http.Client
}

type generateRequest struct {
	Contents []content `json:"contents"`
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
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NewAIClient creates a new Gemini AI client authenticated via service account credentials.
func NewAIClient(ctx context.Context, saFile, project, location, model string) (*Client, error) {
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

//nolint:funlen
func (c *Client) Summarize(ctx context.Context, prompt string) (string, error) {
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

	if len(genResp.Candidates) > 0 && len(genResp.Candidates[0].Content.Parts) > 0 {
		return genResp.Candidates[0].Content.Parts[0].Text, nil
	}

	return "", fmt.Errorf("no summary generated")
}
