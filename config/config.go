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
	OpenRouterToken    string            // API key for OpenRouter
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

	v        *viper.Viper
	reloadMu sync.Mutex

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
	MSTeams    MSTeamsConfig
	GoogleChat GoogleChatConfig
	RocketChat RocketChatConfig
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
	MaxLineLength int

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

	// Quote in reply settings
	QuoteThreaded bool
	QuoteAuthor   string

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

// SharedConfig holds the options that apply to the Mattermost, Matrix, and Slack bridges.
type SharedConfig struct {
	CollapseScrollback bool

	ShowOwnReactions bool

	HideReactions bool

	DisableShowOwnModified bool

	ShowMentions bool

	DisableDefaultMentions bool

	DisableAutoView bool
	ReplayStrategy  string
	EnableLazyJoin  bool
	LazyJoinExclude []string
	ReplayExclude   []string

	JoinReplayDuration        time.Duration
	MaxReplayDuration         time.Duration
	LazyJoinDuration          time.Duration
	HeavySyncThreshold        time.Duration
	DefaultDMOfflineThreshold time.Duration

	HideReplies      bool
	ShortenRepliesTo int

	MessageAttachmentShortFieldMaxLineLength int
	MessageAttachmentOmitFieldTitles         bool

	LastViewedSaveFile string
	PreferNickname     bool
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

	ForceSyncOnReconnect bool
}

type SlackConfig struct {
	SharedConfig

	Bridge    BridgeConfig
	Formatter FormatterConfig

	Disable bool

	SlackclientLogLevel string

	DenyUsers        []string
	UseDisplayName   bool
	UseRealName      bool
	DefaultWorkspace string
}

type MatrixConfig struct {
	SharedConfig

	Bridge    BridgeConfig
	Formatter FormatterConfig

	MatrixclientLogLevel string

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

type MSTeamsConfig struct {
	SharedConfig

	Bridge    BridgeConfig
	Formatter FormatterConfig

	Disable bool

	MsTeamsclientLogLevel string

	AccountType   string // "auto", "work", or "personal"
	TenantID      string
	ClientID      string
	TokenSaveFile string

	SkipTLSVerify        bool
	PrefixMainTeam       bool
	ForceSyncOnReconnect bool
	PollFallback         bool
}

type GoogleChatConfig struct {
	SharedConfig

	Bridge    BridgeConfig
	Formatter FormatterConfig

	Disable bool

	GooglechatclientLogLevel string

	AccountType   string // "auto", "work", or "personal"
	AuthUser      int
	TokenSaveFile string

	SkipTLSVerify        bool
	ForceSyncOnReconnect bool
	PollFallback         bool
}

type RocketChatConfig struct {
	SharedConfig

	Bridge    BridgeConfig
	Formatter FormatterConfig

	RocketchatclientLogLevel string

	Server        string
	SkipTLSVerify bool
	Insecure      bool
	ShowTopic     bool
}

type MastodonConfig struct {
	Bridge    BridgeConfig
	Formatter FormatterConfig

	Server string

	ClientID     string
	ClientSecret string
	AccessToken  string
}

func (c *Config) buildBridgeConfig(section string) BridgeConfig {
	return BridgeConfig{
		Restrict: append([]string(nil), c.v.GetStringSlice(section+".Restrict")...),

		JoinOnly:    append([]string(nil), c.v.GetStringSlice(section+".JoinOnly")...),
		JoinExclude: append([]string(nil), c.v.GetStringSlice(section+".JoinExclude")...),
		JoinInclude: append([]string(nil), c.v.GetStringSlice(section+".JoinInclude")...),
		JoinDM:      c.v.GetBool(section + ".JoinDM"),

		ShowJoinPart:   c.v.GetBool(section + ".ShowJoinPart"),
		ShowOnlyJoined: c.v.GetBool(section + ".ShowOnlyJoined"),
		PartFake:       c.v.GetBool(section + ".PartFake"),

		PrefixContext:    c.v.GetBool(section + ".PrefixContext"),
		SuffixContext:    c.v.GetBool(section + ".SuffixContext"),
		ShowContextMulti: c.v.GetBool(section + ".ShowContextMulti"),
		ThreadContext:    c.v.GetString(section + ".ThreadContext"),

		QuoteThreaded: c.v.GetBool(section + ".QuoteThreaded"),
		QuoteAuthor:   c.v.GetString(section + ".QuoteAuthor"),

		DirectoryURL: c.v.GetString(section + ".DirectoryURL"),
	}
}

func (c *Config) buildSharedConfig(section string) SharedConfig {
	return SharedConfig{
		CollapseScrollback: c.v.GetBool(section + ".CollapseScrollback"),

		ShowOwnReactions: c.v.GetBool(section + ".ShowOwnReactions"),

		HideReactions: c.v.GetBool(section + ".HideReactions"),

		DisableShowOwnModified: c.v.GetBool(section + ".DisableShowOwnModified"),

		ShowMentions: c.v.GetBool(section + ".ShowMentions"),

		DisableDefaultMentions: c.v.GetBool(section + ".DisableDefaultMentions"),

		DisableAutoView: c.v.GetBool(section + ".DisableAutoView"),
		ReplayStrategy:  c.v.GetString(section + ".ReplayStrategy"),
		EnableLazyJoin:  c.v.GetBool(section + ".EnableLazyJoin"),
		LazyJoinExclude: append([]string(nil), c.v.GetStringSlice(section+".LazyJoinExclude")...),
		ReplayExclude:   append([]string(nil), c.v.GetStringSlice(section+".ReplayExclude")...),

		JoinReplayDuration:        c.v.GetDuration(section + ".JoinReplayDuration"),
		MaxReplayDuration:         c.v.GetDuration(section + ".MaxReplayDuration"),
		LazyJoinDuration:          c.v.GetDuration(section + ".LazyJoinDuration"),
		HeavySyncThreshold:        c.v.GetDuration(section + ".HeavySyncThreshold"),
		DefaultDMOfflineThreshold: c.v.GetDuration(section + ".DefaultDMOfflineThreshold"),

		HideReplies:      c.v.GetBool(section + ".HideReplies"),
		ShortenRepliesTo: c.v.GetInt(section + ".ShortenRepliesTo"),

		MessageAttachmentShortFieldMaxLineLength: c.v.GetInt(section + ".MessageAttachmentShortFieldMaxLineLength"),
		MessageAttachmentOmitFieldTitles:         c.v.GetBool(section + ".MessageAttachmentOmitFieldTitles"),

		LastViewedSaveFile: c.v.GetString(section + ".LastViewedSaveFile"),
		PreferNickname:     c.v.GetBool(section + ".PreferNickname"),
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
		OpenRouterToken:    c.v.GetString("ai.openrouter_token"),
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

	mmBridge := c.buildBridgeConfig("mattermost")
	slBridge := c.buildBridgeConfig("slack")
	mdBridge := c.buildBridgeConfig("mastodon")
	mxBridge := c.buildBridgeConfig("matrix")
	mtBridge := c.buildBridgeConfig("msteams")
	gcBridge := c.buildBridgeConfig("googlechat")
	rcBridge := c.buildBridgeConfig("rocketchat")

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
	mtFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("msteams.DisableEmoji"),
		CustomEmoji:  parseCustomEmoji(c.v.GetStringSlice("msteams.CustomEmoji")),

		DisableMarkdown:           c.v.GetBool("msteams.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("msteams.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    unquoteString(c.v.GetString("msteams.MarkdownBlockQuoteChar")),
		MarkdownInlineCode:        unquoteString(c.v.GetString("msteams.MarkdownInlineCode")),

		DisableCodeBlockPrefix: c.v.GetBool("msteams.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        unquoteString(c.v.GetString("msteams.CodeBlockPrefix")),
		CodeBlockSeparator:     unquoteString(c.v.GetString("msteams.CodeBlockSeparator")),
		SyntaxHighlighting:     c.v.GetString("msteams.SyntaxHighlighting"),

		Unicode: c.v.GetBool("msteams.Unicode"),

		EnableIRCHexColors: c.v.GetBool("msteams.EnableIRCHexColors"),

		PreserveNewLines: c.v.GetString("msteams.PreserveNewLines"),
	}
	gcFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("googlechat.DisableEmoji"),
		CustomEmoji:  parseCustomEmoji(c.v.GetStringSlice("googlechat.CustomEmoji")),

		DisableMarkdown:           c.v.GetBool("googlechat.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("googlechat.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    unquoteString(c.v.GetString("googlechat.MarkdownBlockQuoteChar")),
		MarkdownInlineCode:        unquoteString(c.v.GetString("googlechat.MarkdownInlineCode")),

		DisableCodeBlockPrefix: c.v.GetBool("googlechat.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        unquoteString(c.v.GetString("googlechat.CodeBlockPrefix")),
		CodeBlockSeparator:     unquoteString(c.v.GetString("googlechat.CodeBlockSeparator")),
		SyntaxHighlighting:     c.v.GetString("googlechat.SyntaxHighlighting"),

		Unicode: c.v.GetBool("googlechat.Unicode"),

		EnableIRCHexColors: c.v.GetBool("googlechat.EnableIRCHexColors"),

		PreserveNewLines: c.v.GetString("googlechat.PreserveNewLines"),
	}
	rcFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("rocketchat.DisableEmoji"),
		CustomEmoji:  parseCustomEmoji(c.v.GetStringSlice("rocketchat.CustomEmoji")),

		DisableMarkdown:           c.v.GetBool("rocketchat.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("rocketchat.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    unquoteString(c.v.GetString("rocketchat.MarkdownBlockQuoteChar")),
		MarkdownInlineCode:        unquoteString(c.v.GetString("rocketchat.MarkdownInlineCode")),

		DisableCodeBlockPrefix: c.v.GetBool("rocketchat.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        unquoteString(c.v.GetString("rocketchat.CodeBlockPrefix")),
		CodeBlockSeparator:     unquoteString(c.v.GetString("rocketchat.CodeBlockSeparator")),
		SyntaxHighlighting:     c.v.GetString("rocketchat.SyntaxHighlighting"),

		Unicode: c.v.GetBool("rocketchat.Unicode"),

		EnableIRCHexColors: c.v.GetBool("rocketchat.EnableIRCHexColors"),

		PreserveNewLines: c.v.GetString("rocketchat.PreserveNewLines"),
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
			MaxLineLength: c.v.GetInt("MaxLineLength"),

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

			ForceSyncOnReconnect: c.v.GetBool("mattermost.ForceSyncOnReconnect"),
		},

		Slack: SlackConfig{
			Bridge:       slBridge,
			Formatter:    slFormatter,
			SharedConfig: c.buildSharedConfig("slack"),

			Disable: c.v.GetBool("slack.Disable"),

			SlackclientLogLevel: c.v.GetString("slack.SlackclientLogLevel"),

			DenyUsers:        append([]string(nil), c.v.GetStringSlice("slack.DenyUsers")...),
			UseDisplayName:   c.v.GetBool("slack.UseDisplayName"),
			UseRealName:      c.v.GetBool("slack.UseRealName"),
			DefaultWorkspace: c.v.GetString("slack.DefaultWorkspace"),
		},

		Matrix: MatrixConfig{
			Bridge:       mxBridge,
			Formatter:    mxFormatter,
			SharedConfig: c.buildSharedConfig("matrix"),

			MatrixclientLogLevel: c.v.GetString("matrix.MatrixclientLogLevel"),

			DefaultProvider:  c.v.GetString("matrix.DefaultProvider"),
			ReplaceColonChar: c.v.GetString("matrix.ReplaceColonChar"),

			EncryptionDir:       c.v.GetString("matrix.EncryptionDir"),
			EncryptionPickleKey: c.v.GetString("matrix.EncryptionPickleKey"),
		},

		MSTeams: MSTeamsConfig{
			Bridge:       mtBridge,
			Formatter:    mtFormatter,
			SharedConfig: c.buildSharedConfig("msteams"),

			Disable: c.v.GetBool("msteams.Disable"),

			MsTeamsclientLogLevel: c.v.GetString("msteams.MsTeamsclientLogLevel"),

			AccountType:   c.v.GetString("msteams.AccountType"),
			TenantID:      c.v.GetString("msteams.TenantID"),
			ClientID:      c.v.GetString("msteams.ClientID"),
			TokenSaveFile: c.v.GetString("msteams.TokenSaveFile"),

			SkipTLSVerify:        c.v.GetBool("msteams.SkipTLSVerify"),
			PrefixMainTeam:       c.v.GetBool("msteams.PrefixMainTeam"),
			ForceSyncOnReconnect: c.v.GetBool("msteams.ForceSyncOnReconnect"),
			PollFallback:         c.v.GetBool("msteams.PollFallback"),
		},

		GoogleChat: GoogleChatConfig{
			Bridge:       gcBridge,
			Formatter:    gcFormatter,
			SharedConfig: c.buildSharedConfig("googlechat"),

			Disable: c.v.GetBool("googlechat.Disable"),

			GooglechatclientLogLevel: c.v.GetString("googlechat.GooglechatclientLogLevel"),

			AccountType:   c.v.GetString("googlechat.AccountType"),
			AuthUser:      c.v.GetInt("googlechat.AuthUser"),
			TokenSaveFile: c.v.GetString("googlechat.TokenSaveFile"),

			SkipTLSVerify:        c.v.GetBool("googlechat.SkipTLSVerify"),
			ForceSyncOnReconnect: c.v.GetBool("googlechat.ForceSyncOnReconnect"),
			PollFallback:         c.v.GetBool("googlechat.PollFallback"),
		},

		RocketChat: RocketChatConfig{
			Bridge:       rcBridge,
			Formatter:    rcFormatter,
			SharedConfig: c.buildSharedConfig("rocketchat"),

			RocketchatclientLogLevel: c.v.GetString("rocketchat.RocketchatclientLogLevel"),

			Server:        c.v.GetString("rocketchat.Server"),
			SkipTLSVerify: c.v.GetBool("rocketchat.SkipTLSVerify"),
			Insecure:      c.v.GetBool("rocketchat.Insecure"),
			ShowTopic:     c.v.GetBool("rocketchat.ShowTopic"),
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
	case ai.AIOpenRouterProvider:
		if cfgP == ai.AIOpenRouterProvider && c.URL != "" {
			return strings.TrimRight(c.URL, "/")
		}

		return ai.DefaultOpenRouterBaseURL
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
	case ai.AIOpenRouterProvider:
		if c.OpenRouterToken != "" {
			return c.OpenRouterToken
		}

		if strings.EqualFold(c.Provider, ai.AIOpenRouterProvider) {
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

func (c *Config) MSTeams() *MSTeamsConfig {
	return &c.Current().MSTeams
}

func (c *Config) GoogleChat() *GoogleChatConfig {
	return &c.Current().GoogleChat
}

func (c *Config) RocketChat() *RocketChatConfig {
	return &c.Current().RocketChat
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
	v.SetDefault("MaxLineLength", 460)
	v.SetDefault("ServerTime", false)
	v.SetDefault("AwayCooldown", "20m")

	v.SetDefault("mattermost.JoinReplayDuration", "15m")
	v.SetDefault("slack.JoinReplayDuration", "15m")
	v.SetDefault("matrix.JoinReplayDuration", "15m")
	v.SetDefault("msteams.JoinReplayDuration", "15m")
	v.SetDefault("googlechat.JoinReplayDuration", "15m")
	v.SetDefault("matrix.ReplaceColonChar", "[]")
	v.SetDefault("rocketchat.JoinReplayDuration", "15m")

	for _, section := range []string{"mattermost", "slack", "mastodon", "matrix", "msteams", "googlechat", "rocketchat"} {
		v.SetDefault(section+".QuoteThreaded", section != "googlechat")
		v.SetDefault(section+".QuoteAuthor", "displayname")
	}

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
	c.reloadMu.Lock()

	// If the atomic pointer is currently nil, this is the initial startup load
	isFirstLoad := c.Current() == nil

	err := c.v.ReadInConfig()
	if err != nil {
		c.reloadMu.Unlock()
		return err
	}

	err = c.publishRuntimeConfig()
	c.reloadMu.Unlock()

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
