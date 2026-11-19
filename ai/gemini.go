package ai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	geminiModelsCacheTTL = 12 * time.Hour
	geminiMaxListPages   = 10
	geminiListTimeout    = 15 * time.Second
	geminiModelPrefix    = "gemini-"
)

type geminiModelCache struct {
	mu        sync.RWMutex
	key       string
	models    []string
	fetchedAt time.Time
}

var geminiModelsCache geminiModelCache

func (c *geminiModelCache) get(key string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.key != key || len(c.models) == 0 {
		return nil, false
	}

	if time.Since(c.fetchedAt) >= geminiModelsCacheTTL {
		return c.models, false
	}

	return c.models, true
}

func (c *geminiModelCache) set(key string, models []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.key = key
	c.models = models
	c.fetchedAt = time.Now()
}

type geminiListModelsResponse struct {
	Models []struct {
		Name                       string   `json:"name"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
	PublisherModels []struct {
		Name string `json:"name"`
	} `json:"publisherModels"`
	NextPageToken string `json:"nextPageToken"`
}

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
	PromptTokenCount        int `json:"promptTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
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

	cacheHit := max(m.CachedContentTokenCount, 0)
	cacheMiss := max(m.PromptTokenCount-cacheHit, 0)

	res := Usage{
		PromptTokens:     m.PromptTokenCount,
		CacheHitTokens:   cacheHit,
		CacheMissTokens:  cacheMiss,
		CompletionTokens: m.CandidatesTokenCount,
		TotalTokens:      m.TotalTokenCount,
		ReasoningTokens:  m.ThoughtsTokenCount,
	}

	if res.TotalTokens == 0 && (res.PromptTokens > 0 || res.CompletionTokens > 0) {
		res.TotalTokens = res.PromptTokens + res.CompletionTokens + res.ReasoningTokens
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

func (c *GeminiClient) vertexEndpointHost() string {
	switch c.location {
	case "", "global", "us":
		return "aiplatform.googleapis.com"
	case "eu":
		return "eu-aiplatform.googleapis.com"
	default:
		return fmt.Sprintf("%s-aiplatform.googleapis.com", c.location)
	}
}

func (c *GeminiClient) modelCacheKey() string {
	if c.apiKey != "" {
		return "apikey:" + strings.TrimRight(c.BaseURL(), "/")
	}

	return "vertex:" + c.vertexEndpointHost()
}

func shortGeminiModelName(name string) string {
	if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
		name = name[idx+1:]
	}

	return strings.ToLower(strings.TrimSpace(name))
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func extractGeminiPrefix(modelName string) string {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	if !strings.HasPrefix(lower, geminiModelPrefix) {
		return ""
	}

	i := len(geminiModelPrefix)
	if i >= len(lower) || !isASCIIDigit(lower[i]) {
		return ""
	}

	for i < len(lower) {
		if isASCIIDigit(lower[i]) {
			i++

			continue
		}

		if lower[i] == '.' && i+1 < len(lower) && isASCIIDigit(lower[i+1]) {
			i++

			continue
		}

		break
	}

	if i < len(lower) && lower[i] == '-' {
		return lower[:i+1]
	}

	return ""
}

func parseGeminiVersion(modelName string) ([4]int, int) {
	var parts [4]int

	rest, ok := strings.CutPrefix(modelName, geminiModelPrefix)
	if !ok || len(rest) == 0 || !isASCIIDigit(rest[0]) {
		return parts, 0
	}

	i := 0
	count := 0

	for i < len(rest) && count < len(parts) {
		val := 0
		for i < len(rest) && isASCIIDigit(rest[i]) {
			val = val*10 + int(rest[i]-'0')
			i++
		}

		parts[count] = val
		count++

		if i+1 < len(rest) && rest[i] == '.' && isASCIIDigit(rest[i+1]) {
			i++

			continue
		}

		break
	}

	return parts, count
}

func compareGeminiVersions(a [4]int, aLen int, b [4]int, bLen int) int {
	n := min(aLen, bLen)
	for i := range n {
		if c := cmp.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}

	return cmp.Compare(aLen, bLen)
}

func geminiModelBetter(a, b string) bool {
	aStable := !strings.Contains(a, "-preview") && !strings.Contains(a, "-exp")
	bStable := !strings.Contains(b, "-preview") && !strings.Contains(b, "-exp")

	if aStable != bStable {
		return aStable
	}

	aVer, aLen := parseGeminiVersion(a)
	bVer, bLen := parseGeminiVersion(b)

	if c := compareGeminiVersions(aVer, aLen, bVer, bLen); c != 0 {
		return c > 0
	}

	return len(a) < len(b)
}

func pickBetterGeminiModel(best, candidate string) string {
	if best == "" || geminiModelBetter(candidate, best) {
		return candidate
	}

	return best
}

func matchGeminiModel(query string, models []string) string {
	if len(models) == 0 {
		return ""
	}

	prefixed := geminiModelPrefix + query
	suffix := "-" + query
	token := "-" + query + "-"

	var (
		prefixedMatch string
		suffixBest    string
		tokenBest     string
	)

	for _, m := range models {
		if m == query {
			return m
		}

		if m == prefixed {
			prefixedMatch = m
		}

		if strings.HasSuffix(m, suffix) {
			suffixBest = pickBetterGeminiModel(suffixBest, m)
		}

		if strings.Contains(m, token) {
			tokenBest = pickBetterGeminiModel(tokenBest, m)
		}
	}

	switch {
	case prefixedMatch != "":
		return prefixedMatch
	case suffixBest != "":
		return suffixBest
	case tokenBest != "":
		return tokenBest
	default:
		return ""
	}
}

func fallbackGeminiModel(query, defaultModel string) string {
	if strings.HasPrefix(query, geminiModelPrefix) {
		return query
	}

	if len(query) > 0 && isASCIIDigit(query[0]) {
		return geminiModelPrefix + query
	}

	prefix := extractGeminiPrefix(defaultModel)
	if prefix == "" {
		prefix = extractGeminiPrefix(DefaultGeminiModel)
	}

	if prefix == "" {
		prefix = "gemini-4-"
	}

	return prefix + query
}

func appendGeminiModels(names []string, seen map[string]struct{}, listResp *geminiListModelsResponse) []string {
	for _, m := range listResp.Models {
		if len(m.SupportedGenerationMethods) > 0 && !slices.Contains(m.SupportedGenerationMethods, "generateContent") {
			continue
		}

		shortName := shortGeminiModelName(m.Name)
		if !strings.HasPrefix(shortName, geminiModelPrefix) {
			continue
		}

		if _, ok := seen[shortName]; ok {
			continue
		}

		seen[shortName] = struct{}{}
		names = append(names, shortName)
	}

	for _, m := range listResp.PublisherModels {
		shortName := shortGeminiModelName(m.Name)
		if !strings.HasPrefix(shortName, geminiModelPrefix) {
			continue
		}

		if _, ok := seen[shortName]; ok {
			continue
		}

		seen[shortName] = struct{}{}
		names = append(names, shortName)
	}

	return names
}

func (c *GeminiClient) fetchModelPage(ctx context.Context, reqURL, authHeader, authValue string) (*geminiListModelsResponse, error) {
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set(authHeader, authValue)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models list API returned status %s", resp.Status)
	}

	var listResp geminiListModelsResponse

	err = json.NewDecoder(resp.Body).Decode(&listResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode models list response: %w", err)
	}

	return &listResp, nil
}

func (c *GeminiClient) listModels(ctx context.Context) ([]string, error) {
	var (
		baseEndpoint string
		authHeader   = "Authorization"
		authValue    string
	)

	switch {
	case c.apiKey != "":
		baseEndpoint = strings.TrimRight(c.BaseURL(), "/") + "/models"
		authHeader = "x-goog-api-key"
		authValue = c.apiKey
	case c.tokenSource != nil:
		token, err := c.tokenSource.Token()
		if err != nil {
			return nil, fmt.Errorf("failed to get oauth2 token: %w", err)
		}

		baseEndpoint = fmt.Sprintf("https://%s/v1beta1/publishers/google/models", c.vertexEndpointHost())
		authValue = "Bearer " + token.AccessToken
	default:
		return nil, errors.New("gemini client has neither API key nor service account credentials configured")
	}

	var (
		names     []string
		seen      = make(map[string]struct{})
		pageToken string
	)

	for range geminiMaxListPages {
		reqURL := baseEndpoint + "?pageSize=100"
		if pageToken != "" {
			reqURL += "&pageToken=" + url.QueryEscape(pageToken)
		}

		listResp, err := c.fetchModelPage(ctx, reqURL, authHeader, authValue)
		if err != nil {
			return nil, err
		}

		names = appendGeminiModels(names, seen, listResp)

		if listResp.NextPageToken == "" || listResp.NextPageToken == pageToken {
			break
		}

		pageToken = listResp.NextPageToken
	}

	return names, nil
}

func (c *GeminiClient) getAvailableModels(ctx context.Context, forceRefresh bool) ([]string, bool) {
	cacheKey := c.modelCacheKey()

	cached, valid := geminiModelsCache.get(cacheKey)
	if !forceRefresh && valid {
		return cached, true
	}

	listCtx, cancel := context.WithTimeout(ctx, geminiListTimeout)
	defer cancel()

	models, err := c.listModels(listCtx)
	if err != nil {
		logWarnf("Unable to list Gemini models: %v", err)

		return cached, false
	}

	if len(models) > 0 {
		geminiModelsCache.set(cacheKey, models)

		return models, false
	}

	return cached, false
}

// ResolveModel resolves a Gemini model name or shorthand against available models
// (cached for 12h), falling back to prefix normalization derived from fallbackModel,
// and updates the client's active model to the resolved ID.
func (c *GeminiClient) ResolveModel(ctx context.Context, fallbackModel string) string {
	trimmed := strings.TrimSpace(c.model)
	if trimmed == "" {
		trimmed = strings.TrimSpace(fallbackModel)
	}

	if trimmed == "" {
		trimmed = DefaultGeminiModel
	}

	if strings.Contains(trimmed, "/") {
		c.model = trimmed

		return c.model
	}

	query := strings.ToLower(trimmed)

	models, fromCache := c.getAvailableModels(ctx, false)
	matched := matchGeminiModel(query, models)

	if matched == "" && fromCache {
		models, _ = c.getAvailableModels(ctx, true)
		matched = matchGeminiModel(query, models)
	}

	if matched != "" {
		c.model = matched

		return c.model
	}

	c.model = fallbackGeminiModel(query, fallbackModel)

	return c.model
}

//nolint:funlen,gocyclo
func (c *GeminiClient) Summarize(ctx context.Context, prompt string, thinking string) (*SummaryResult, error) {
	var (
		endpointURL string
		authHeader  = "Authorization"
		authValue   string
	)

	switch {
	case c.apiKey != "":
		baseURL := strings.TrimRight(c.BaseURL(), "/")
		modelID := strings.TrimPrefix(c.model, "publishers/google/models/")

		if strings.HasPrefix(modelID, "models/") {
			endpointURL = fmt.Sprintf("%s/%s:generateContent", baseURL, modelID)
		} else {
			endpointURL = fmt.Sprintf("%s/models/%s:generateContent", baseURL, modelID)
		}

		authHeader = "x-goog-api-key"
		authValue = c.apiKey
	case c.tokenSource != nil:
		token, err := c.tokenSource.Token()
		if err != nil {
			return nil, fmt.Errorf("failed to get oauth2 token: %w", err)
		}

		endpointHost := c.vertexEndpointHost()

		switch {
		case strings.HasPrefix(c.model, "projects/"):
			endpointURL = fmt.Sprintf("https://%s/v1/%s:generateContent", endpointHost, c.model)
		case strings.HasPrefix(c.model, "publishers/"):
			endpointURL = fmt.Sprintf(
				"https://%s/v1/projects/%s/locations/%s/%s:generateContent",
				endpointHost, c.project, c.location, c.model,
			)
		default:
			modelID := strings.TrimPrefix(c.model, "models/")
			endpointURL = fmt.Sprintf(
				"https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
				endpointHost, c.project, c.location, modelID,
			)
		}

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

	resp, err := doPostWithRetry(ctx, c.httpClient, "gemini", endpointURL, headers, rawJSON)
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
