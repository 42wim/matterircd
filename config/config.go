package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/42wim/matterircd/ai"
	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	logger   *logrus.Entry
	LogLevel string

	UserAgent string
)

type AIConfig struct {
	Enabled            bool
	Provider           string            // "gemini" (default), "copilot", or "openai"
	Token              string            // Default token for configured Provider
	ClaudeToken        string            // API key for Anthropic Claude
	CopilotToken       string            // GitHub token for Copilot
	DeepSeekToken      string            // API key for DeepSeek
	GeminiToken        string            // API key for Google AI Studio
	HetznerToken       string            // API key for Hetzner
	MuseToken          string            // API key for Meta Muse
	OpenAIToken        string            // API key for OpenAI
	XAIToken           string            // API key for xAI (Grok)
	URL                string            // Fallback base URL for configured Provider
	URLs               map[string]string // Provider-specific base URLs
	ServiceAccountFile string            // Gemini / Vertex AI
	Project            string
	Location           string
	Model              string            // Fallback model if not specified in Models map
	Models             map[string]string // Provider-specific models, e.g. { "gemini": "gemini-4-hydrogen", "copilot": "gpt-6-luna" }
	Prompt             string
	DefaultPostLimit   int
	MaxPostLimit       int
}

type Config struct {
	current atomic.Pointer[RuntimeConfig]

	v *viper.Viper

	reloadHooks    []reloadHook
	nextReloadHook uint64
	hooksMu        sync.RWMutex
}

type reloadHook struct {
	id uint64
	fn func(rc *RuntimeConfig)
}

// RuntimeConfig is immutable.
//
// A new RuntimeConfig is constructed whenever the configuration is
// reloaded and atomically published. Callers must never modify it.
type RuntimeConfig struct {
	GlobalConfig

	AI AIConfig

	Mattermost MattermostConfig
	Slack      SlackConfig
	Mastodon   MastodonConfig
	Matrix     MatrixConfig
}

type GlobalConfig struct {
	Bind string

	Debug         bool
	Trace         bool
	Gops          bool
	Profiling     bool
	ProfilingBind string

	TLSBind string
	TLSDir  string
	TLSKey  string
	TLSCert string

	HandshakeTimeout int
	ClientTimeout    int

	PasteBufferTimeout int

	HardLineSplit bool

	EnableTyping      bool
	DisableSendTyping bool
	ServerTime        bool

	AwayCooldown time.Duration
}

type BridgeConfig struct {
	Restrict []string

	JoinOnly    []string
	JoinExclude []string
	JoinInclude []string
	JoinDM      bool

	ShowJoinPart   bool
	ShowOnlyJoined bool
	PartFake       bool

	// Threading / context related
	PrefixContext    bool
	SuffixContext    bool
	ThreadContext    string
	ShowContextMulti bool

	DirectoryURL string
}

type FormatterConfig struct {
	DisableEmoji bool
	CustomEmoji  map[string]string

	DisableMarkdown           bool
	DisableMarkdownBlockQuote bool
	MarkdownBlockQuoteChar    string
	MarkdownInlineCode        string

	SyntaxHighlighting     string
	DisableCodeBlockPrefix bool
	CodeBlockPrefix        string
	CodeBlockSeparator     string

	Unicode bool

	EnableIRCHexColors bool

	PreserveNewLines string
}

// SharedConfig holds the options that apply to both the Mattermost and Matrix bridges.
type SharedConfig struct {
	CollapseScrollback bool

	ShowOwnReactions bool

	ShowMentions bool

	DisableDefaultMentions bool

	DisableAutoView bool
	ReplayStrategy  string
	EnableLazyJoin  bool
	LazyJoinExclude []string
	ReplayExclude   []string

	JoinReplayDuration time.Duration
	MaxReplayDuration  time.Duration
	LazyJoinDuration   time.Duration
}

type MattermostConfig struct {
	SharedConfig

	Bridge    BridgeConfig
	Formatter FormatterConfig

	MatterclientLogLevel string

	DefaultServer string
	DefaultTeam   string

	Insecure            bool
	IgnoreServerVersion bool

	SkipTLSVerify bool

	PrefixMainTeam bool

	ForceAntiIdle    bool
	AntiIdleChannel  string
	AntiIdleInterval int

	PreferNickname bool

	HideReplies      bool
	ShortenRepliesTo int

	HideReactions bool

	DisableShowOwnModified bool

	LastViewedSaveFile   string
	ForceSyncOnReconnect bool

	DefaultDMOfflineThreshold time.Duration
	HeavySyncThreshold        time.Duration

	MessageAttachmentShortFieldMaxLineLength int
	MessageAttachmentOmitFieldTitles         bool
}

type SlackConfig struct {
	Bridge    BridgeConfig
	Formatter FormatterConfig

	Disable bool

	SlackclientLogLevel string

	DenyUsers      []string
	UseDisplayName bool
	PreferNickname bool
	UseRealName    bool
}

type MatrixConfig struct {
	SharedConfig

	Bridge    BridgeConfig
	Formatter FormatterConfig

	MatrixclientLogLevel string

	// Only used when mattermost.LastViewedSaveFile is unset, there is one file for all bridges.
	LastViewedSaveFile string

	HideReplies      bool
	ShortenRepliesTo int

	// DefaultProvider is left out of channel names, e.g. "ubuntu.com".
	DefaultProvider string

	// ReplaceColonChar separates localpart and homeserver in the nick of remote
	// users instead of Matrix's ':', e.g. "/" gives hloeung/matrix.org. The
	// special value "[]" gives hloeung[matrix.org].
	ReplaceColonChar string

	// Encrypted rooms, needs a build with -tags goolm and cgo.
	EncryptionDir       string
	EncryptionPickleKey string
}

type MastodonConfig struct {
	Bridge    BridgeConfig
	Formatter FormatterConfig

	Server string

	ClientID     string
	ClientSecret string
	AccessToken  string
}

func (c *Config) buildSharedConfig(section string) SharedConfig {
	return SharedConfig{
		CollapseScrollback: c.v.GetBool(section + ".CollapseScrollback"),

		ShowOwnReactions: c.v.GetBool(section + ".ShowOwnReactions"),

		ShowMentions: c.v.GetBool(section + ".ShowMentions"),

		DisableDefaultMentions: c.v.GetBool(section + ".DisableDefaultMentions"),

		DisableAutoView: c.v.GetBool(section + ".DisableAutoView"),
		ReplayStrategy:  c.v.GetString(section + ".ReplayStrategy"),
		EnableLazyJoin:  c.v.GetBool(section + ".EnableLazyJoin"),
		LazyJoinExclude: append([]string(nil), c.v.GetStringSlice(section+".LazyJoinExclude")...),
		ReplayExclude:   append([]string(nil), c.v.GetStringSlice(section+".ReplayExclude")...),

		JoinReplayDuration: c.v.GetDuration(section + ".JoinReplayDuration"),
		MaxReplayDuration:  c.v.GetDuration(section + ".MaxReplayDuration"),
		LazyJoinDuration:   c.v.GetDuration(section + ".LazyJoinDuration"),
	}
}

//nolint:funlen
func (c *Config) buildRuntimeCfg() *RuntimeConfig {
	aiURL := c.v.GetString("ai.url")
	if aiURL == "" {
		aiURL = c.v.GetString("ai.base_url")
	}

	claudeToken := c.v.GetString("ai.claude_token")
	if claudeToken == "" {
		claudeToken = c.v.GetString("ai.anthropic_token")
	}

	geminiToken := c.v.GetString("ai.gemini_token")
	if geminiToken == "" {
		geminiToken = c.v.GetString("ai.google_token")
	}

	museToken := c.v.GetString("ai.muse_token")
	if museToken == "" {
		museToken = c.v.GetString("ai.meta_token")
	}

	xaiToken := c.v.GetString("ai.xai_token")
	if xaiToken == "" {
		xaiToken = c.v.GetString("ai.grok_token")
	}

	aiCfg := AIConfig{
		Enabled:            c.v.GetBool("ai.enabled"),
		Provider:           c.v.GetString("ai.provider"),
		Token:              c.v.GetString("ai.token"),
		ClaudeToken:        claudeToken,
		CopilotToken:       c.v.GetString("ai.copilot_token"),
		DeepSeekToken:      c.v.GetString("ai.deepseek_token"),
		GeminiToken:        geminiToken,
		HetznerToken:       c.v.GetString("ai.hetzner_token"),
		MuseToken:          museToken,
		OpenAIToken:        c.v.GetString("ai.openai_token"),
		XAIToken:           xaiToken,
		URL:                unquoteString(aiURL),
		URLs:               c.v.GetStringMapString("ai.urls"),
		ServiceAccountFile: unquoteString(c.v.GetString("ai.service_account_file")),
		Project:            c.v.GetString("ai.project"),
		Location:           c.v.GetString("ai.location"),
		Model:              c.v.GetString("ai.model"),
		Models:             c.v.GetStringMapString("ai.models"),
		Prompt:             c.v.GetString("ai.prompt"),
		DefaultPostLimit:   c.v.GetInt("ai.default_post_limit"),
		MaxPostLimit:       c.v.GetInt("ai.max_post_limit"),
	}

	mmBridge := BridgeConfig{
		Restrict: append([]string(nil), c.v.GetStringSlice("mattermost.Restrict")...),

		JoinOnly:    append([]string(nil), c.v.GetStringSlice("mattermost.JoinOnly")...),
		JoinExclude: append([]string(nil), c.v.GetStringSlice("mattermost.JoinExclude")...),
		JoinInclude: append([]string(nil), c.v.GetStringSlice("mattermost.JoinInclude")...),
		JoinDM:      c.v.GetBool("mattermost.JoinDM"),

		ShowJoinPart:   c.v.GetBool("mattermost.ShowJoinPart"),
		ShowOnlyJoined: c.v.GetBool("mattermost.ShowOnlyJoined"),
		PartFake:       c.v.GetBool("mattermost.PartFake"),

		PrefixContext:    c.v.GetBool("mattermost.PrefixContext"),
		SuffixContext:    c.v.GetBool("mattermost.SuffixContext"),
		ShowContextMulti: c.v.GetBool("mattermost.ShowContextMulti"),
		ThreadContext:    c.v.GetString("mattermost.ThreadContext"),

		DirectoryURL: c.v.GetString("mattermost.DirectoryURL"),
	}
	slBridge := BridgeConfig{
		Restrict: append([]string(nil), c.v.GetStringSlice("slack.Restrict")...),

		JoinOnly:    append([]string(nil), c.v.GetStringSlice("slack.JoinOnly")...),
		JoinExclude: append([]string(nil), c.v.GetStringSlice("slack.JoinExclude")...),
		JoinInclude: append([]string(nil), c.v.GetStringSlice("slack.JoinInclude")...),
		JoinDM:      c.v.GetBool("slack.JoinDM"),

		ShowJoinPart:   c.v.GetBool("slack.ShowJoinPart"),
		ShowOnlyJoined: c.v.GetBool("slack.ShowOnlyJoined"),
		PartFake:       c.v.GetBool("slack.PartFake"),

		PrefixContext:    c.v.GetBool("slack.PrefixContext"),
		SuffixContext:    c.v.GetBool("slack.SuffixContext"),
		ShowContextMulti: c.v.GetBool("slack.ShowContextMulti"),
		ThreadContext:    c.v.GetString("slack.ThreadContext"),

		DirectoryURL: c.v.GetString("slack.DirectoryURL"),
	}
	mdBridge := BridgeConfig{
		Restrict: append([]string(nil), c.v.GetStringSlice("mastodon.Restrict")...),

		JoinOnly:    append([]string(nil), c.v.GetStringSlice("mastodon.JoinOnly")...),
		JoinExclude: append([]string(nil), c.v.GetStringSlice("mastodon.JoinExclude")...),
		JoinInclude: append([]string(nil), c.v.GetStringSlice("mastodon.JoinInclude")...),
		JoinDM:      c.v.GetBool("mastodon.JoinDM"),

		ShowJoinPart:   c.v.GetBool("mastodon.ShowJoinPart"),
		ShowOnlyJoined: c.v.GetBool("mastodon.ShowOnlyJoined"),
		PartFake:       c.v.GetBool("mastodon.PartFake"),

		PrefixContext:    c.v.GetBool("mastodon.PrefixContext"),
		SuffixContext:    c.v.GetBool("mastodon.SuffixContext"),
		ShowContextMulti: c.v.GetBool("mastodon.ShowContextMulti"),
		ThreadContext:    c.v.GetString("mastodon.ThreadContext"),

		DirectoryURL: c.v.GetString("mastodon.DirectoryURL"),
	}

	mxBridge := BridgeConfig{
		Restrict: append([]string(nil), c.v.GetStringSlice("matrix.Restrict")...),

		JoinOnly:    append([]string(nil), c.v.GetStringSlice("matrix.JoinOnly")...),
		JoinExclude: append([]string(nil), c.v.GetStringSlice("matrix.JoinExclude")...),
		JoinInclude: append([]string(nil), c.v.GetStringSlice("matrix.JoinInclude")...),
		JoinDM:      c.v.GetBool("matrix.JoinDM"),

		ShowJoinPart:   c.v.GetBool("matrix.ShowJoinPart"),
		ShowOnlyJoined: c.v.GetBool("matrix.ShowOnlyJoined"),
		PartFake:       c.v.GetBool("matrix.PartFake"),

		PrefixContext:    c.v.GetBool("matrix.PrefixContext"),
		SuffixContext:    c.v.GetBool("matrix.SuffixContext"),
		ShowContextMulti: c.v.GetBool("matrix.ShowContextMulti"),
		ThreadContext:    c.v.GetString("matrix.ThreadContext"),

		DirectoryURL: c.v.GetString("matrix.DirectoryURL"),
	}

	mmFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("mattermost.DisableEmoji"),
		CustomEmoji:  parseCustomEmoji(c.v.GetStringSlice("mattermost.CustomEmoji")),

		DisableMarkdown:           c.v.GetBool("mattermost.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("mattermost.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    unquoteString(c.v.GetString("mattermost.MarkdownBlockQuoteChar")),
		MarkdownInlineCode:        unquoteString(c.v.GetString("mattermost.MarkdownInlineCode")),

		DisableCodeBlockPrefix: c.v.GetBool("mattermost.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        unquoteString(c.v.GetString("mattermost.CodeBlockPrefix")),
		CodeBlockSeparator:     unquoteString(c.v.GetString("mattermost.CodeBlockSeparator")),
		SyntaxHighlighting:     c.v.GetString("mattermost.SyntaxHighlighting"),

		Unicode: c.v.GetBool("mattermost.Unicode"),

		EnableIRCHexColors: c.v.GetBool("mattermost.EnableIRCHexColors"),

		PreserveNewLines: c.v.GetString("mattermost.PreserveNewLines"),
	}
	slFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("slack.DisableEmoji"),
		CustomEmoji:  parseCustomEmoji(c.v.GetStringSlice("slack.CustomEmoji")),

		DisableMarkdown:           c.v.GetBool("slack.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("slack.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    unquoteString(c.v.GetString("slack.MarkdownBlockQuoteChar")),
		MarkdownInlineCode:        unquoteString(c.v.GetString("slack.MarkdownInlineCode")),

		DisableCodeBlockPrefix: c.v.GetBool("slack.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        unquoteString(c.v.GetString("slack.CodeBlockPrefix")),
		CodeBlockSeparator:     unquoteString(c.v.GetString("slack.CodeBlockSeparator")),
		SyntaxHighlighting:     c.v.GetString("slack.SyntaxHighlighting"),

		Unicode: c.v.GetBool("slack.Unicode"),

		EnableIRCHexColors: c.v.GetBool("slack.EnableIRCHexColors"),

		PreserveNewLines: c.v.GetString("slack.PreserveNewLines"),
	}
	mdFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("mastodon.DisableEmoji"),
		CustomEmoji:  parseCustomEmoji(c.v.GetStringSlice("mastodon.CustomEmoji")),

		DisableMarkdown:           c.v.GetBool("mastodon.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("mastodon.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    unquoteString(c.v.GetString("mastodon.MarkdownBlockQuoteChar")),
		MarkdownInlineCode:        unquoteString(c.v.GetString("mastodon.MarkdownInlineCode")),

		DisableCodeBlockPrefix: c.v.GetBool("mastodon.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        unquoteString(c.v.GetString("mastodon.CodeBlockPrefix")),
		CodeBlockSeparator:     unquoteString(c.v.GetString("mastodon.CodeBlockSeparator")),
		SyntaxHighlighting:     c.v.GetString("mastodon.SyntaxHighlighting"),

		Unicode: c.v.GetBool("mastodon.Unicode"),

		EnableIRCHexColors: c.v.GetBool("mastodon.EnableIRCHexColors"),

		PreserveNewLines: c.v.GetString("mastodon.PreserveNewLines"),
	}
	mxFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("matrix.DisableEmoji"),
		CustomEmoji:  parseCustomEmoji(c.v.GetStringSlice("matrix.CustomEmoji")),

		DisableMarkdown:           c.v.GetBool("matrix.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("matrix.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    unquoteString(c.v.GetString("matrix.MarkdownBlockQuoteChar")),
		MarkdownInlineCode:        unquoteString(c.v.GetString("matrix.MarkdownInlineCode")),

		DisableCodeBlockPrefix: c.v.GetBool("matrix.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        unquoteString(c.v.GetString("matrix.CodeBlockPrefix")),
		CodeBlockSeparator:     unquoteString(c.v.GetString("matrix.CodeBlockSeparator")),
		SyntaxHighlighting:     c.v.GetString("matrix.SyntaxHighlighting"),

		Unicode: c.v.GetBool("matrix.Unicode"),

		EnableIRCHexColors: c.v.GetBool("matrix.EnableIRCHexColors"),

		PreserveNewLines: c.v.GetString("matrix.PreserveNewLines"),
	}

	return &RuntimeConfig{
		GlobalConfig: GlobalConfig{
			Bind: c.v.GetString("bind"),

			Debug:         c.v.GetBool("debug"),
			Trace:         c.v.GetBool("trace"),
			Gops:          c.v.GetBool("gops"),
			Profiling:     c.v.GetBool("profiling"),
			ProfilingBind: c.v.GetString("profilingbind"),

			TLSBind: c.v.GetString("tlsbind"),
			TLSDir:  c.v.GetString("tlsdir"),
			TLSKey:  c.v.GetString("tlskey"),
			TLSCert: c.v.GetString("tlscert"),

			HandshakeTimeout: c.v.GetInt("HandshakeTimeout"),
			ClientTimeout:    c.v.GetInt("ClientTimeout"),

			PasteBufferTimeout: c.v.GetInt("PasteBufferTimeout"),

			HardLineSplit: c.v.GetBool("HardLineSplit"),

			EnableTyping:      c.v.GetBool("EnableTyping"),
			DisableSendTyping: c.v.GetBool("DisableSendTyping"),
			ServerTime:        c.v.GetBool("ServerTime"),

			AwayCooldown: c.v.GetDuration("AwayCooldown"),
		},

		AI: aiCfg,

		Mattermost: MattermostConfig{
			Bridge:       mmBridge,
			Formatter:    mmFormatter,
			SharedConfig: c.buildSharedConfig("mattermost"),

			MatterclientLogLevel: c.v.GetString("mattermost.MatterclientLogLevel"),

			DefaultServer: c.v.GetString("mattermost.DefaultServer"),
			DefaultTeam:   c.v.GetString("mattermost.DefaultTeam"),

			Insecure:            c.v.GetBool("mattermost.Insecure"),
			IgnoreServerVersion: c.v.GetBool("mattermost.IgnoreServerVersion"),

			SkipTLSVerify: c.v.GetBool("mattermost.SkipTLSVerify"),

			PrefixMainTeam: c.v.GetBool("mattermost.PrefixMainTeam"),

			ForceAntiIdle:    c.v.GetBool("mattermost.ForceAntiIdle"),
			AntiIdleChannel:  c.v.GetString("mattermost.AntiIdleChannel"),
			AntiIdleInterval: c.v.GetInt("mattermost.AntiIdleInterval"),

			PreferNickname: c.v.GetBool("mattermost.PreferNickname"),

			HideReplies:      c.v.GetBool("mattermost.HideReplies"),
			ShortenRepliesTo: c.v.GetInt("mattermost.ShortenRepliesTo"),

			HideReactions: c.v.GetBool("mattermost.HideReactions"),

			DisableShowOwnModified: c.v.GetBool("mattermost.DisableShowOwnModified"),

			LastViewedSaveFile:   c.v.GetString("mattermost.LastViewedSaveFile"),
			ForceSyncOnReconnect: c.v.GetBool("mattermost.ForceSyncOnReconnect"),

			DefaultDMOfflineThreshold: c.v.GetDuration("mattermost.DefaultDMOfflineThreshold"),
			HeavySyncThreshold:        c.v.GetDuration("mattermost.HeavySyncThreshold"),

			MessageAttachmentShortFieldMaxLineLength: c.v.GetInt("mattermost.MessageAttachmentShortFieldMaxLineLength"),
			MessageAttachmentOmitFieldTitles:         c.v.GetBool("mattermost.MessageAttachmentOmitFieldTitles"),
		},

		Slack: SlackConfig{
			Bridge:    slBridge,
			Formatter: slFormatter,

			Disable: c.v.GetBool("slack.Disable"),

			SlackclientLogLevel: c.v.GetString("slack.SlackclientLogLevel"),

			DenyUsers:      append([]string(nil), c.v.GetStringSlice("slack.DenyUsers")...),
			UseDisplayName: c.v.GetBool("slack.UseDisplayName"),
			PreferNickname: c.v.GetBool("slack.PreferNickname"),
			UseRealName:    c.v.GetBool("slack.UseRealName"),
		},

		Matrix: MatrixConfig{
			Bridge:       mxBridge,
			Formatter:    mxFormatter,
			SharedConfig: c.buildSharedConfig("matrix"),

			MatrixclientLogLevel: c.v.GetString("matrix.MatrixclientLogLevel"),

			LastViewedSaveFile: c.v.GetString("matrix.LastViewedSaveFile"),

			HideReplies:      c.v.GetBool("matrix.HideReplies"),
			ShortenRepliesTo: c.v.GetInt("matrix.ShortenRepliesTo"),
			DefaultProvider:  c.v.GetString("matrix.DefaultProvider"),
			ReplaceColonChar: c.v.GetString("matrix.ReplaceColonChar"),

			EncryptionDir:       c.v.GetString("matrix.EncryptionDir"),
			EncryptionPickleKey: c.v.GetString("matrix.EncryptionPickleKey"),
		},

		Mastodon: MastodonConfig{
			Bridge:    mdBridge,
			Formatter: mdFormatter,

			Server: c.v.GetString("mastodon.server"),

			ClientID:     c.v.GetString("mastodon.clientid"),
			ClientSecret: c.v.GetString("mastodon.clientsecret"),
			AccessToken:  c.v.GetString("mastodon.accesstoken"),
		},
	}
}

func (c *Config) AI() *AIConfig {
	return &c.Current().AI
}

// GetBaseURL resolves the endpoint base URL for the requested provider, ensuring
// credentials and requests are not dispatched to unrelated endpoints.
//
//nolint:funlen,gocyclo,gocognit
func (c *AIConfig) GetBaseURL(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))

	if c.URLs != nil {
		if u, ok := c.URLs[p]; ok && u != "" {
			return strings.TrimRight(u, "/")
		}

		if p == ai.AIClaudeProvider && c.URLs[ai.AIAnthropicProvider] != "" {
			return strings.TrimRight(c.URLs[ai.AIAnthropicProvider], "/")
		} else if p == ai.AIAnthropicProvider && c.URLs[ai.AIClaudeProvider] != "" {
			return strings.TrimRight(c.URLs[ai.AIClaudeProvider], "/")
		}
	}

	cfgP := strings.ToLower(strings.TrimSpace(c.Provider))

	switch p {
	case ai.AIClaudeProvider, ai.AIAnthropicProvider:
		if (cfgP == ai.AIClaudeProvider || cfgP == ai.AIAnthropicProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultClaudeBaseURL
	case ai.AIDeepSeekProvider:
		if cfgP == ai.AIDeepSeekProvider && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultDeepSeekBaseURL
	case ai.AIGeminiProvider, ai.AIGoogleProvider:
		if (cfgP == ai.AIGeminiProvider || cfgP == ai.AIGoogleProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultGeminiBaseURL
	case ai.AIHetznerProvider:
		if cfgP == ai.AIHetznerProvider && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultHetznerBaseURL
	case ai.AIMuseProvider, ai.AIMetaProvider:
		if (cfgP == ai.AIMuseProvider || cfgP == ai.AIMetaProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultMuseBaseURL
	case ai.AIOpenAIProvider:
		if cfgP == ai.AIOpenAIProvider && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultOpenAIBaseURL
	case ai.AIOllamaProvider, ai.AILocalProvider, ai.AIvLLMProvider:
		if (cfgP == p || cfgP == ai.AILocalProvider || cfgP == ai.AIOllamaProvider || cfgP == ai.AIvLLMProvider || cfgP == "" || cfgP == ai.AIGeminiProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ""
	case ai.AIXAIProvider, ai.AIGrokProvider:
		if (cfgP == ai.AIXAIProvider || cfgP == ai.AIGrokProvider) && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultXAIBaseURL
	default:
		return ""
	}
}

// GetToken resolves the token for the requested provider, ensuring credentials
// do not cross provider or endpoint boundaries while preserving the legacy Copilot token fallback.
//
//nolint:funlen,gocyclo,gocognit
func (c *AIConfig) GetToken(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))

	switch p {
	case ai.AIClaudeProvider, ai.AIAnthropicProvider:
		if c.ClaudeToken != "" {
			return c.ClaudeToken
		}

		if strings.EqualFold(c.Provider, ai.AIClaudeProvider) || strings.EqualFold(c.Provider, ai.AIAnthropicProvider) {
			return c.Token
		}
	case ai.AICopilotProvider, ai.AIGitHubProvider:
		if c.CopilotToken != "" {
			return c.CopilotToken
		}

		cfgP := strings.ToLower(c.Provider)
		if cfgP == "" || (cfgP == ai.AIGeminiProvider && c.ServiceAccountFile != "") || cfgP == ai.AICopilotProvider || cfgP == ai.AIGitHubProvider {
			return c.Token
		}
	case ai.AIDeepSeekProvider:
		if c.DeepSeekToken != "" {
			return c.DeepSeekToken
		}

		if strings.EqualFold(c.Provider, ai.AIDeepSeekProvider) {
			return c.Token
		}
	case ai.AIGeminiProvider, ai.AIGoogleProvider:
		if c.GeminiToken != "" {
			return c.GeminiToken
		}

		if strings.EqualFold(c.Provider, ai.AIGeminiProvider) || strings.EqualFold(c.Provider, ai.AIGoogleProvider) {
			return c.Token
		}
	case ai.AIHetznerProvider:
		if c.HetznerToken != "" {
			return c.HetznerToken
		}

		if strings.EqualFold(c.Provider, ai.AIHetznerProvider) {
			return c.Token
		}
	case ai.AIMuseProvider, ai.AIMetaProvider:
		if c.MuseToken != "" {
			return c.MuseToken
		}

		if strings.EqualFold(c.Provider, ai.AIMuseProvider) || strings.EqualFold(c.Provider, ai.AIMetaProvider) {
			return c.Token
		}
	case ai.AIOpenAIProvider:
		if c.OpenAIToken != "" {
			return c.OpenAIToken
		}

		if strings.EqualFold(c.Provider, ai.AIOpenAIProvider) {
			return c.Token
		}
	case ai.AIOllamaProvider, ai.AILocalProvider, ai.AIvLLMProvider:
		if strings.EqualFold(c.Provider, p) {
			return c.Token
		}
	case ai.AIXAIProvider, ai.AIGrokProvider:
		if c.XAIToken != "" {
			return c.XAIToken
		}

		if strings.EqualFold(c.Provider, ai.AIXAIProvider) || strings.EqualFold(c.Provider, ai.AIGrokProvider) {
			return c.Token
		}
	}

	return ""
}

func (c *Config) Current() *RuntimeConfig {
	return c.current.Load()
}

func (c *Config) Mattermost() *MattermostConfig {
	return &c.Current().Mattermost
}

func (c *Config) Slack() *SlackConfig {
	return &c.Current().Slack
}

func (c *Config) Matrix() *MatrixConfig {
	return &c.Current().Matrix
}

func (c *Config) Mastodon() *MastodonConfig {
	return &c.Current().Mastodon
}

func Load(cfgfile string, flags *pflag.FlagSet) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(cfgfile)

	v.SetEnvPrefix("matterircd")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	// use environment variables
	v.AutomaticEnv()

	v.SetDefault("HardLineSplit", true)
	v.SetDefault("ServerTime", false)
	v.SetDefault("AwayCooldown", "20m")

	v.SetDefault("mattermost.JoinReplayDuration", "15m")
	v.SetDefault("matrix.JoinReplayDuration", "15m")
	v.SetDefault("matrix.ReplaceColonChar", "[]")

	c := &Config{
		v: v,
	}

	err := c.v.BindPFlags(flags)
	if err != nil {
		return nil, err
	}

	_, err = os.Stat(cfgfile)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("unable to access config file %q: %w", cfgfile, err)
		}

		err = c.publishRuntimeConfig()
		if err != nil {
			return nil, err
		}

		return c, nil
	}

	err = c.Reload()
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	c.watch()

	return c, nil
}

func (c *Config) publishRuntimeConfig() error {
	runtimeCfg := c.buildRuntimeCfg()

	err := validate(runtimeCfg)
	if err != nil {
		return err
	}

	c.current.Store(runtimeCfg)

	return nil
}

// RegisterReloadHook allows other packages to execute logic when the config file changes.
//
// The returned function unregisters the hook again. A hook keeps everything it
// references alive, so hooks of things with a limited lifetime (like a login
// session) must be unregistered when that lifetime ends.
func (c *Config) RegisterReloadHook(hook func(rc *RuntimeConfig)) func() {
	c.hooksMu.Lock()
	defer c.hooksMu.Unlock()

	id := c.nextReloadHook
	c.nextReloadHook++
	c.reloadHooks = append(c.reloadHooks, reloadHook{id: id, fn: hook})

	var once sync.Once

	return func() {
		once.Do(func() {
			c.hooksMu.Lock()
			defer c.hooksMu.Unlock()

			// Build a new slice, Reload might be running a snapshot of the old one.
			hooks := make([]reloadHook, 0, len(c.reloadHooks))
			for _, h := range c.reloadHooks {
				if h.id != id {
					hooks = append(hooks, h)
				}
			}

			c.reloadHooks = hooks
		})
	}
}

// Reload re-reads the config file, updates the atomic pointer, and triggers hooks.
func (c *Config) Reload() error {
	// If the atomic pointer is currently nil, this is the initial startup load
	isFirstLoad := c.Current() == nil

	err := c.v.ReadInConfig()
	if err != nil {
		return err
	}

	err = c.publishRuntimeConfig()
	if err != nil {
		return err
	}

	// Only print the reload message if this isn't the initial boot
	if !isFirstLoad && logger != nil {
		logger.Info("configuration reloaded")
	}

	// Successfully reloaded, trigger all registered hooks safely.
	// Run them from a snapshot without holding the lock: a hook is allowed to
	// (un)register hooks itself.
	c.hooksMu.RLock()
	hooks := c.reloadHooks
	c.hooksMu.RUnlock()

	rc := c.Current()
	for _, hook := range hooks {
		hook.fn(rc)
	}

	return nil
}

func SetLogger(l *logrus.Entry) {
	logger = l
}

func SetLogLevel(level logrus.Level) {
	if logger != nil {
		logger.Logger.SetLevel(level)
	}
}

func unquoteString(s string) string {
	if s == "" || !strings.ContainsRune(s, '\\') {
		return s
	}

	unq, err := strconv.Unquote(`"` + s + `"`)
	if err == nil {
		return unq
	}

	return s
}

func validate(runtimeCfg *RuntimeConfig) error {
	return nil
}

func (c *Config) watch() {
	if runtime.GOOS == "illumos" {
		return
	}

	var (
		mu    sync.Mutex
		timer *time.Timer
	)

	// reload config on file changes
	c.v.OnConfigChange(func(_ fsnotify.Event) {
		mu.Lock()
		defer mu.Unlock()

		// If a timer is already running, stop it (debounce)
		if timer != nil {
			timer.Stop()
		}

		// Wait 500ms after the last filesystem event before reloading
		timer = time.AfterFunc(500*time.Millisecond, func() {
			err := c.Reload()
			if err != nil && logger != nil {
				logger.WithError(err).Error("config reload failed")
			}
		})
	})

	c.v.WatchConfig()
}

func parseCustomEmoji(slice []string) map[string]string {
	if len(slice) == 0 {
		return nil
	}

	m := make(map[string]string, len(slice))
	for _, s := range slice {
		if k, v, found := strings.Cut(s, "="); found {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	return m
}
