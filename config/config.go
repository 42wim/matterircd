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
	Provider           string // "gemini" (default) or "copilot"
	Token              string // GitHub token for Copilot
	ServiceAccountFile string // Gemini / Vertex AI
	Project            string
	Location           string
	Model              string            // Fallback model if not specified in Models map
	Models             map[string]string // Provider-specific models, e.g. { "gemini": "gemini-3.7-flash", "copilot": "gpt-4o" }
	Prompt             string
	DefaultPostLimit   int
	MaxPostLimit       int
}

type Config struct {
	current atomic.Pointer[RuntimeConfig]

	v *viper.Viper

	reloadHooks []func(rc *RuntimeConfig)
	hooksMu     sync.RWMutex
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

	EnableTyping     bool
	DisableSendTyping bool
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

type MattermostConfig struct {
	Bridge    BridgeConfig
	Formatter FormatterConfig

	MatterclientLogLevel string

	DefaultServer string
	DefaultTeam   string

	Insecure            bool
	IgnoreServerVersion bool

	CollapseScrollback bool

	SkipTLSVerify bool

	PrefixMainTeam bool

	ForceAntiIdle    bool
	AntiIdleChannel  string
	AntiIdleInterval int

	PreferNickname bool

	HideReplies      bool
	ShortenRepliesTo int

	HideReactions    bool
	ShowOwnReactions bool

	ShowMentions bool

	DisableDefaultMentions bool
	DisableShowOwnModified bool

	DisableAutoView      bool
	LastViewedSaveFile   string
	ReplayStrategy       string
	EnableLazyJoin       bool
	LazyJoinExclude      []string
	ForceSyncOnReconnect bool

	MaxReplayDuration         time.Duration
	LazyJoinDuration          time.Duration
	DefaultDMOfflineThreshold time.Duration
	HeavySyncThreshold        time.Duration

	MessageAttachmentShortFieldMaxLineLength int
	MessageAttachmentOmitFieldTitles         bool
}

type SlackConfig struct {
	Bridge    BridgeConfig
	Formatter FormatterConfig

	Disable bool

	DenyUsers      []string
	UseDisplayName bool
	PreferNickname bool
}

type MastodonConfig struct {
	Bridge    BridgeConfig
	Formatter FormatterConfig

	Server string

	ClientID     string
	ClientSecret string
	AccessToken  string
}

//nolint:funlen
func (c *Config) buildRuntimeCfg() *RuntimeConfig {
	aiCfg := AIConfig{
		Enabled:            c.v.GetBool("ai.enabled"),
		Provider:           c.v.GetString("ai.provider"),
		Token:              c.v.GetString("ai.token"),
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

			EnableTyping:      c.v.GetBool("EnableTyping"),
			DisableSendTyping: c.v.GetBool("DisableSendTyping"),
		},

		AI: aiCfg,

		Mattermost: MattermostConfig{
			Bridge:    mmBridge,
			Formatter: mmFormatter,

			MatterclientLogLevel: c.v.GetString("mattermost.MatterclientLogLevel"),

			DefaultServer: c.v.GetString("mattermost.DefaultServer"),
			DefaultTeam:   c.v.GetString("mattermost.DefaultTeam"),

			Insecure:            c.v.GetBool("mattermost.Insecure"),
			IgnoreServerVersion: c.v.GetBool("mattermost.IgnoreServerVersion"),

			CollapseScrollback: c.v.GetBool("mattermost.CollapseScrollback"),

			SkipTLSVerify: c.v.GetBool("mattermost.SkipTLSVerify"),

			PrefixMainTeam: c.v.GetBool("mattermost.PrefixMainTeam"),

			ForceAntiIdle:    c.v.GetBool("mattermost.ForceAntiIdle"),
			AntiIdleChannel:  c.v.GetString("mattermost.AntiIdleChannel"),
			AntiIdleInterval: c.v.GetInt("mattermost.AntiIdleInterval"),

			PreferNickname: c.v.GetBool("mattermost.PreferNickname"),

			HideReplies:      c.v.GetBool("mattermost.HideReplies"),
			ShortenRepliesTo: c.v.GetInt("mattermost.ShortenRepliesTo"),

			HideReactions:    c.v.GetBool("mattermost.HideReactions"),
			ShowOwnReactions: c.v.GetBool("mattermost.ShowOwnReactions"),

			ShowMentions: c.v.GetBool("mattermost.ShowMentions"),

			DisableDefaultMentions: c.v.GetBool("mattermost.DisableDefaultMentions"),
			DisableShowOwnModified: c.v.GetBool("mattermost.DisableShowOwnModified"),

			DisableAutoView:      c.v.GetBool("mattermost.DisableAutoView"),
			LastViewedSaveFile:   c.v.GetString("mattermost.LastViewedSaveFile"),
			ReplayStrategy:       c.v.GetString("mattermost.ReplayStrategy"),
			EnableLazyJoin:       c.v.GetBool("mattermost.EnableLazyJoin"),
			LazyJoinExclude:      append([]string(nil), c.v.GetStringSlice("mattermost.LazyJoinExclude")...),
			ForceSyncOnReconnect: c.v.GetBool("mattermost.ForceSyncOnReconnect"),

			MaxReplayDuration:         c.v.GetDuration("mattermost.MaxReplayDuration"),
			LazyJoinDuration:          c.v.GetDuration("mattermost.LazyJoinDuration"),
			DefaultDMOfflineThreshold: c.v.GetDuration("mattermost.DefaultDMOfflineThreshold"),
			HeavySyncThreshold:        c.v.GetDuration("mattermost.HeavySyncThreshold"),

			MessageAttachmentShortFieldMaxLineLength: c.v.GetInt("mattermost.MessageAttachmentShortFieldMaxLineLength"),
			MessageAttachmentOmitFieldTitles:         c.v.GetBool("mattermost.MessageAttachmentOmitFieldTitles"),
		},

		Slack: SlackConfig{
			Bridge:    slBridge,
			Formatter: slFormatter,

			Disable: c.v.GetBool("slack.Disable"),

			DenyUsers:      append([]string(nil), c.v.GetStringSlice("slack.DenyUsers")...),
			UseDisplayName: c.v.GetBool("slack.UseDisplayName"),
			PreferNickname: c.v.GetBool("slack.PreferNickname"),
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

func (c *Config) Current() *RuntimeConfig {
	return c.current.Load()
}

func (c *AIConfig) GetModel(provider string) string {
	p := strings.ToLower(provider)
	if c.Models != nil {
		if m, ok := c.Models[p]; ok && m != "" {
			return m
		}
	}

	if c.Model != "" {
		return c.Model
	}

	switch p {
	case "copilot", "github":
		return "gpt-4o-mini"
	default:
		return "gemini-3.8-flash"
	}
}

func (c *Config) Mattermost() *MattermostConfig {
	return &c.Current().Mattermost
}

func (c *Config) Slack() *SlackConfig {
	return &c.Current().Slack
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

	c := &Config{
		v: v,
	}

	if err := c.v.BindPFlags(flags); err != nil {
		return nil, err
	}

	if _, err := os.Stat(cfgfile); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("unable to access config file %q: %w", cfgfile, err)
		}

		if err := c.publishRuntimeConfig(); err != nil {
			return nil, err
		}

		return c, nil
	}

	if err := c.Reload(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	c.watch()

	return c, nil
}

func (c *Config) publishRuntimeConfig() error {
	runtimeCfg := c.buildRuntimeCfg()

	if err := validate(runtimeCfg); err != nil {
		return err
	}

	c.current.Store(runtimeCfg)

	return nil
}

// RegisterReloadHook allows other packages to execute logic when the config file changes.
func (c *Config) RegisterReloadHook(hook func(rc *RuntimeConfig)) {
	c.hooksMu.Lock()
	defer c.hooksMu.Unlock()
	c.reloadHooks = append(c.reloadHooks, hook)
}

// Reload re-reads the config file, updates the atomic pointer, and triggers hooks.
func (c *Config) Reload() error {
	// If the atomic pointer is currently nil, this is the initial startup load
	isFirstLoad := c.Current() == nil

	if err := c.v.ReadInConfig(); err != nil {
		return err
	}

	if err := c.publishRuntimeConfig(); err != nil {
		return err
	}

	// Only print the reload message if this isn't the initial boot
	if !isFirstLoad && logger != nil {
		logger.Info("configuration reloaded")
	}

	// Successfully reloaded, trigger all registered hooks safely
	c.hooksMu.RLock()
	rc := c.Current()
	for _, hook := range c.reloadHooks {
		hook(rc)
	}
	c.hooksMu.RUnlock()

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
	if unq, err := strconv.Unquote(`"` + s + `"`); err == nil {
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
			if err := c.Reload(); err != nil && logger != nil {
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
