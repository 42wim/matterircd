//go:build !noai

package irckit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/42wim/matterircd/ai"
	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/utils"
)

type summarizeQuery struct {
	Count int
	Since int64

	Provider string
	Model    string
	Thinking string
}

func (u *User) isBareSummarizePostID(target string) bool {
	if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "@") {
		return false
	}

	if len(target) == 26 {
		return true
	}

	if u.br == nil {
		return false
	}

	proto := u.br.Protocol()

	return (proto == serviceSlack || proto == serviceMsTeams || proto == serviceGoogleChat) && u.br.NormalizePostID(target) != ""
}

func (u *User) getSummarizeEvents(target string, query summarizeQuery) (string, []*bridge.Event, error) {
	target = strings.TrimSpace(target)

	// Post / Thread ID (@@<id> or bare post/thread ID)
	postID, isThread := strings.CutPrefix(target, "@@")
	isBarePostID := u.isBareSummarizePostID(target)

	if isThread || isBarePostID {
		if !isThread {
			postID = target
		}

		events := u.br.GetPostThread(u.ctx, postID)
		if len(events) == 0 {
			return "", nil, fmt.Errorf("thread '@@%s' not found", postID)
		}

		return "thread " + postID, events, nil
	}

	// Channel (#...)
	if channelName, ok := strings.CutPrefix(target, "#"); ok {
		teamID := u.br.GetMe().TeamID
		channelID := u.br.GetChannelID(u.ctx, channelName, teamID)

		brchannel, err := u.br.GetChannel(u.ctx, channelID)
		if err != nil {
			return "", nil, fmt.Errorf("channel '#%s' not found", channelName)
		}

		var events []*bridge.Event
		if query.Since > 0 {
			events = u.br.GetPostsSince(u.ctx, brchannel.ID, query.Since)
		} else {
			events = u.br.GetPosts(u.ctx, brchannel.ID, query.Count)
		}

		return "#" + channelName, events, nil
	}

	// Direct Message (user or @user)
	userName := strings.TrimPrefix(target, "@")

	channelID := u.br.GetChannelID(u.ctx, userName, "")

	brchannel, err := u.br.GetChannel(u.ctx, channelID)
	if err != nil {
		return "", nil, fmt.Errorf("user '@%s' not found", userName)
	}

	var events []*bridge.Event
	if query.Since > 0 {
		events = u.br.GetPostsSince(u.ctx, brchannel.ID, query.Since)
	} else {
		events = u.br.GetPosts(u.ctx, brchannel.ID, query.Count)
	}

	return "@" + userName, events, nil
}

func parseSummarizeAIProvider(arg string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case ai.AIClaudeProvider, ai.AIAnthropicProvider:
		return ai.AIClaudeProvider, true
	case ai.AICopilotProvider, ai.AIGitHubProvider:
		return ai.AICopilotProvider, true
	case ai.AIGeminiProvider, ai.AIGoogleProvider:
		return ai.AIGeminiProvider, true
	case ai.AIXAIProvider, ai.AIGrokProvider:
		return ai.AIXAIProvider, true
	case ai.AIMuseProvider, ai.AIMetaProvider:
		return ai.AIMuseProvider, true
	case ai.AIOpenAIProvider, ai.AIOpenRouterProvider, ai.AIDeepSeekProvider, ai.AIHetznerProvider, ai.AIOllamaProvider, ai.AILocalProvider, ai.AIvLLMProvider:
		return strings.ToLower(strings.TrimSpace(arg)), true
	default:
		return "", false
	}
}

func parseSummarizeAIThinkingMode(arg string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "off", "none": //nolint:goconst
		return "off", true
	case "low":
		return "low", true
	case "medium", "med":
		return "medium", true
	case "high":
		return "high", true
	case "auto":
		return "auto", true
	default:
		return "", false
	}
}

const invalidSummarizeOptionMsg = "invalid option: expected post count (e.g. 50), duration (e.g. 24h), thinking mode (off, low, medium, high), provider (claude, copilot, deepseek, gemini, muse, openai, openrouter, xai), or model (e.g. gemini-4-hydrogen)"

func hasNonEmptyPrefix(s, prefix string) bool {
	return strings.HasPrefix(s, prefix) && len(s) > len(prefix)
}

func inferSummarizeAIProviderFromModel(model string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(model))

	switch {
	case hasNonEmptyPrefix(m, "gemini-"),
		hasNonEmptyPrefix(m, "models/gemini-"),
		hasNonEmptyPrefix(m, "publishers/google/models/gemini-"):
		return ai.AIGeminiProvider, true
	case hasNonEmptyPrefix(m, "claude-"):
		return ai.AIClaudeProvider, true
	case hasNonEmptyPrefix(m, "deepseek-"):
		return ai.AIDeepSeekProvider, true
	case hasNonEmptyPrefix(m, "grok-"):
		return ai.AIXAIProvider, true
	case hasNonEmptyPrefix(m, "muse-"):
		return ai.AIMuseProvider, true
	default:
		return "", false
	}
}

//nolint:funlen,gocyclo,gocognit
func parseSummarizeLimit(extraArgs []string, defaultLimit, maxLimit int) (summarizeQuery, error) {
	if maxLimit == 0 {
		maxLimit = 100
	}

	if defaultLimit == 0 {
		defaultLimit = 30
	}

	query := summarizeQuery{Count: defaultLimit}
	hasLimit := false

	for _, arg := range extraArgs {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		if provider, ok := parseSummarizeAIProvider(arg); ok {
			if query.Provider != "" {
				return summarizeQuery{}, errors.New("provider specified more than once")
			}

			query.Provider = provider

			continue
		}

		if mode, ok := parseSummarizeAIThinkingMode(arg); ok {
			if query.Thinking != "" {
				return summarizeQuery{}, errors.New("thinking mode specified more than once")
			}

			query.Thinking = mode

			continue
		}

		// Try parsing as a time duration (e.g. 24h, 30m)
		d, err := time.ParseDuration(arg)
		if err == nil {
			if hasLimit {
				return summarizeQuery{}, errors.New("limit or duration specified more than once")
			}

			if d <= 0 {
				return summarizeQuery{}, errors.New("duration must be positive")
			}

			query.Since = time.Now().Add(-d).UnixMilli()
			query.Count = 0
			hasLimit = true

			continue
		}

		// Try parsing as an integer count (e.g. 50)
		val, err := strconv.Atoi(arg)
		if err == nil {
			if val <= 0 {
				return summarizeQuery{}, errors.New("post count must be positive")
			}

			if hasLimit {
				return summarizeQuery{}, errors.New("limit or duration specified more than once")
			}

			if val > maxLimit {
				val = maxLimit
			}

			query.Count = val
			hasLimit = true

			continue
		}

		if query.Model != "" {
			if query.Provider == "" {
				if _, ok := inferSummarizeAIProviderFromModel(query.Model); !ok {
					return summarizeQuery{}, errors.New(invalidSummarizeOptionMsg)
				}
			}

			return summarizeQuery{}, errors.New("model specified more than once")
		}

		query.Model = arg
	}

	if query.Model != "" && query.Provider == "" {
		provider, ok := inferSummarizeAIProviderFromModel(query.Model)
		if !ok {
			return summarizeQuery{}, errors.New(invalidSummarizeOptionMsg)
		}

		query.Provider = provider
	}

	return query, nil
}

//nolint:funlen,gocognit,gocyclo
func summarize(u *User, toUser *User, args []string, service string) {
	cfg := u.cfg.Current().AI
	if !cfg.Enabled {
		u.MsgUser(toUser, "AI summarization is not enabled.")

		return
	}

	if len(args) == 0 || len(args) > 5 {
		u.MsgUser(toUser, "Usage: SUMMARIZE <#channel | user | @@thread_id> [post_count | duration] [thinking] [provider [model] | model]")
		u.MsgUser(toUser, "e.g. SUMMARIZE #dev 50, SUMMARIZE #dev 24h gemini high, SUMMARIZE #dev 1h gemini hydrogen, SUMMARIZE #dev 1h gemini-4-hydrogen, SUMMARIZE @@q3kz9... openai")

		return
	}

	query, err := parseSummarizeLimit(args[1:], cfg.DefaultPostLimit, cfg.MaxPostLimit)
	if err != nil {
		u.MsgUser(toUser, err.Error())

		return
	}

	provider := query.Provider
	if provider == "" {
		provider = cfg.Provider
	}

	if provider == "" {
		provider = ai.AIGeminiProvider
	}

	defaultModel := ai.GetModel(provider, cfg.Model, cfg.Models)

	model := defaultModel
	if query.Model != "" {
		model = query.Model
	}

	token := cfg.GetToken(provider)

	var (
		aiClient ai.Summarizer
		project  = cfg.Project
	)

	switch strings.ToLower(provider) {
	case ai.AIClaudeProvider, ai.AIAnthropicProvider:
		if token == "" {
			u.MsgUser(toUser, "Claude AI is not configured (missing token).")

			return
		}

		baseURL := cfg.GetBaseURL(provider)

		aiClient, err = ai.NewClaudeClient(token, baseURL, model)
		if err != nil {
			u.MsgUser(toUser, fmt.Sprintf("Failed to initialize Claude client: %v", err))

			return
		}
	case ai.AICopilotProvider, ai.AIGitHubProvider:
		if token == "" {
			u.MsgUser(toUser, "Copilot AI is not configured (missing token).")

			return
		}

		aiClient, err = ai.NewCopilotClient(token, model)
		if err != nil {
			u.MsgUser(toUser, fmt.Sprintf("Failed to initialize Copilot AI: %v", err))

			return
		}
	case ai.AIOpenRouterProvider:
		if token == "" {
			u.MsgUser(toUser, "OpenRouter AI is not configured (missing token).")

			return
		}

		baseURL := cfg.GetBaseURL(provider)

		aiClient, err = ai.NewOpenRouterClient(token, baseURL, model)
		if err != nil {
			u.MsgUser(toUser, fmt.Sprintf("Failed to initialize OpenRouter client: %v", err))

			return
		}
	case ai.AIOpenAIProvider, ai.AIDeepSeekProvider, ai.AIXAIProvider, ai.AIGrokProvider, ai.AIMuseProvider, ai.AIMetaProvider, ai.AIHetznerProvider, ai.AIOllamaProvider, ai.AILocalProvider, ai.AIvLLMProvider:
		baseURL := cfg.GetBaseURL(provider)
		if baseURL == "" {
			u.MsgUser(toUser, fmt.Sprintf("%s AI is not configured (missing url).", provider))

			return
		}

		if token == "" && (strings.Contains(baseURL, "api.deepseek.com") || strings.Contains(baseURL, "api.meta.ai") || strings.Contains(baseURL, "api.openai.com") || strings.Contains(baseURL, "api.x.ai") || strings.Contains(baseURL, "inference.hetzner.com")) {
			u.MsgUser(toUser, fmt.Sprintf("%s AI is not configured (missing token).", provider))

			return
		}

		aiClient, err = ai.NewOpenAIClient(token, baseURL, model)
		if err != nil {
			u.MsgUser(toUser, fmt.Sprintf("Failed to initialize OpenAI client: %v", err))

			return
		}
	default:
		switch {
		case cfg.ServiceAccountFile != "":
			geminiClient, err := ai.NewGeminiClient(u.ctx, cfg.ServiceAccountFile, cfg.Project, cfg.Location, model)
			if err != nil {
				u.MsgUser(toUser, fmt.Sprintf("Failed to initialize Gemini AI: %v", err))

				return
			}

			project = geminiClient.Project()
			aiClient = geminiClient
		case token != "":
			baseURL := cfg.GetBaseURL(provider)

			geminiClient, err := ai.NewGeminiAPIKeyClient(token, baseURL, model)
			if err != nil {
				u.MsgUser(toUser, fmt.Sprintf("Failed to initialize Gemini AI: %v", err))

				return
			}

			aiClient = geminiClient
		default:
			u.MsgUser(toUser, "Gemini AI is not configured (missing service_account_file or token).")

			return
		}
	}

	contextLabel, events, err := u.getSummarizeEvents(args[0], query)
	if err != nil {
		u.MsgUser(toUser, err.Error())

		return
	}

	if len(events) == 0 {
		u.MsgUser(toUser, "No messages found to summarize.")

		return
	}

	if geminiClient, ok := aiClient.(*ai.GeminiClient); ok {
		model = geminiClient.ResolveModel(u.ctx, defaultModel)
	}

	ellipsis := "..."
	if u.br.FormatterConfig().Unicode {
		ellipsis = "…"
	}

	thinkingStr := ""
	if query.Thinking != "" && query.Thinking != "off" && query.Thinking != "none" {
		thinkingStr = ", thinking: " + query.Thinking
	}

	u.MsgUser(toUser, fmt.Sprintf("Fetching messages for %s and generating summary (provider: %s, model: %s%s) %s", contextLabel, provider, model, thinkingStr, ellipsis))

	var transcript strings.Builder

	for _, event := range events {
		switch e := event.Data.(type) {
		case *bridge.ChannelMessageEvent:
			if e.Sender != nil && e.Text != "" {
				fmt.Fprintf(&transcript, "%s: %s\n", u.sanitizeNick(e.Sender.Nick), e.Text)
			}
		case *bridge.DirectMessageEvent:
			if e.Sender != nil && e.Text != "" {
				fmt.Fprintf(&transcript, "%s: %s\n", u.sanitizeNick(e.Sender.Nick), e.Text)
			}
		}
	}

	extraInstructions := ""
	if trimmed := strings.TrimSpace(cfg.Prompt); trimmed != "" {
		extraInstructions = trimmed + " "
	}

	prompt := fmt.Sprintf(
		"You are an assistant summarizing chat history for an IRC client. "+
			"Summarize the following %s conversation. "+
			"%sHighlight decisions, action items, and main points. Prefix users with '@'. "+
			"Do not include a title or Markdown tables.\n\n"+
			"[TRANSCRIPT]\n%s[/TRANSCRIPT]\n",
		contextLabel, extraInstructions, transcript.String(),
	)

	thinking := query.Thinking
	if thinking == "" {
		thinking = "none"
	}

	switch strings.ToLower(provider) {
	case ai.AIClaudeProvider, ai.AIAnthropicProvider:
		baseURL := cfg.GetBaseURL(provider)

		logger.Infof("AI summarization for %s requested by %s (provider: %s, model: %s, thinking: %s, url: %s)", contextLabel, u.Nick, provider, model, thinking, baseURL)
	case ai.AICopilotProvider, ai.AIGitHubProvider:
		logger.Infof("AI summarization for %s requested by %s (provider: %s, model: %s, thinking: %s)", contextLabel, u.Nick, provider, model, thinking)
	case ai.AIOpenAIProvider, ai.AIOpenRouterProvider, ai.AIDeepSeekProvider, ai.AIXAIProvider, ai.AIGrokProvider, ai.AIMuseProvider, ai.AIMetaProvider, ai.AIHetznerProvider, ai.AIOllamaProvider, ai.AILocalProvider, ai.AIvLLMProvider:
		baseURL := cfg.GetBaseURL(provider)

		logger.Infof("AI summarization for %s requested by %s (provider: %s, model: %s, thinking: %s, url: %s)", contextLabel, u.Nick, provider, model, thinking, baseURL)
	default:
		if cfg.ServiceAccountFile == "" {
			baseURL := cfg.GetBaseURL(provider)

			logger.Infof("AI summarization for %s requested by %s (provider: %s, model: %s, thinking: %s, url: %s)", contextLabel, u.Nick, provider, model, thinking, baseURL)

			break
		}

		region := cfg.Location
		if region == "" {
			region = "us"
		}

		logger.Infof(
			"AI summarization for %s requested by %s (provider: %s, model: %s, thinking: %s, project: %s, region: %s)",
			contextLabel, u.Nick, provider, model, thinking, project, region,
		)
	}

	startTime := time.Now()

	summaryResult, err := aiClient.Summarize(u.ctx, prompt, query.Thinking)
	if err != nil {
		logger.Warnf("AI summarization failed for %s requested by %s: %v", contextLabel, u.Nick, err)
		u.MsgUser(toUser, fmt.Sprintf("Summarization failed: %v", err))

		return
	}

	duration := time.Since(startTime)

	heading := "Summary for " + contextLabel
	headingLen := 4 + len(heading) + 4

	u.MsgUser(toUser, "=== \x02\x1d"+heading+"\x1d\x02 ===")

	disableEmoji := u.br.FormatterConfig().DisableEmoji
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode
	blockQuoteChar, codeBlockPrefix := u.getMarkdownBlockCodePrefix()
	syntaxHighlighting := u.br.FormatterConfig().SyntaxHighlighting
	codeBlockSeparator := u.br.FormatterConfig().CodeBlockSeparator
	preserveNewLines := u.br.FormatterConfig().PreserveNewLines

	opts := utils.ProcessMessageOpts{
		DisableMarkdown:    disableMarkdown,
		DisableEmoji:       disableEmoji,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
		PreserveNewLines:   preserveNewLines,
	}

	utils.ProcessMessageText(summaryResult.Content, opts, func(line string) {
		trimmed := strings.TrimRight(line, " \r\t")
		if trimmed != "" {
			u.MsgUser(toUser, trimmed)
		}
	})

	u.MsgUser(toUser, strings.Repeat("=", headingLen))

	stats := formatSummarizeStats(len(events), transcript.Len(), model, summaryResult, duration)
	u.MsgUser(toUser, stats)
	logger.Infof("AI summarization completed for %s requested by %s %s", contextLabel, u.Nick, stats)
}

func formatSummarizeStats(postCount, contentBytes int, requestedModel string, result *ai.SummaryResult, duration time.Duration) string {
	var parts []string

	postStr := fmt.Sprintf("%d posts", postCount)
	if postCount == 1 {
		postStr = "1 post"
	}

	var sizeStr string
	if contentBytes < 1024 {
		sizeStr = fmt.Sprintf("%d B", contentBytes)
	} else {
		sizeStr = fmt.Sprintf("%.1f KB", float64(contentBytes)/1024.0)
	}

	parts = append(parts, postStr+", "+sizeStr)

	if result != nil {
		resolvedModel := strings.TrimSpace(result.Model)
		if resolvedModel != "" && !strings.EqualFold(resolvedModel, requestedModel) {
			parts = append(parts, "Model: "+resolvedModel)
		}

		if result.Usage.TotalTokens > 0 || result.Usage.PromptTokens > 0 || result.Usage.CompletionTokens > 0 {
			parts = append(parts, formatTokenUsage(result.Usage))
		}

		if result.Usage.Cost > 0 {
			parts = append(parts, "Cost: "+formatSummarizeCost(result.Usage.Cost))
		}
	}

	parts = append(parts, fmt.Sprintf("%.1fs", duration.Seconds()))

	return fmt.Sprintf("[%s]", strings.Join(parts, " | "))
}

func formatSummarizeCost(cost float64) string {
	switch {
	case cost < 0.000001:
		return "<$0.000001"
	case cost < 0.0001:
		return fmt.Sprintf("$%.6f", cost)
	case cost < 0.01:
		return fmt.Sprintf("$%.5f", cost)
	default:
		return fmt.Sprintf("$%.4f", cost)
	}
}

func formatTokenUsage(usage ai.Usage) string {
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return fmt.Sprintf("Tokens: %s total", formatTokenNumber(usage.TotalTokens))
	}

	cacheHit := max(usage.CacheHitTokens, 0)

	cacheMiss := usage.CacheMissTokens
	if cacheMiss == 0 && usage.PromptTokens > cacheHit {
		cacheMiss = usage.PromptTokens - cacheHit
	}

	inputStr := fmt.Sprintf("%s input (%s cache hit, %s cache miss)",
		formatTokenNumber(usage.PromptTokens),
		formatTokenNumber(cacheHit),
		formatTokenNumber(cacheMiss),
	)

	outputStr := fmt.Sprintf("%s output", formatTokenNumber(usage.CompletionTokens))
	if usage.ReasoningTokens > 0 {
		outputStr = fmt.Sprintf("%s output (thinking: %s)",
			formatTokenNumber(usage.CompletionTokens),
			formatTokenNumber(usage.ReasoningTokens),
		)
	}

	return fmt.Sprintf("Tokens: %s + %s = %s total",
		inputStr,
		outputStr,
		formatTokenNumber(usage.TotalTokens),
	)
}

func formatTokenNumber(n int) string {
	if n < 0 {
		return "-" + formatTokenNumber(-n)
	}

	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}

	var b strings.Builder

	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}

	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}

		b.WriteString(s[i : i+3])
	}

	return b.String()
}
