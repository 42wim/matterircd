package config

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	logger   *logrus.Entry
	LogLevel string
)

type Config struct {
	current atomic.Pointer[RuntimeConfig]

	v *viper.Viper
}

// RuntimeConfig is immutable.
//
// A new RuntimeConfig is constructed whenever the configuration is
// reloaded and atomically published. Callers must never modify it.
type RuntimeConfig struct {
	GlobalConfig

	Mattermost MattermostConfig
	Slack      SlackConfig
	Mastodon   MastodonConfig
}

type GlobalConfig struct {
	Bind string

	Debug bool
	Trace bool
	Gops  bool

	TLSBind string
	TLSDir  string
	TLSKey  string
	TLSCert string

	HandshakeTimeout int
	ClientTimeout    int

	PasteBufferTimeout int
}

type BridgeConfig struct {
	Restrict []string

	JoinOnly    []string
	JoinExclude []string
	JoinInclude []string

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

	DisableMarkdown           bool
	DisableMarkdownBlockQuote bool
	MarkdownBlockQuoteChar    string
	MarkdownInlineCode        string

	SyntaxHighlighting     string
	DisableCodeBlockPrefix bool
	CodeBlockPrefix        string

	Unicode bool
}

type MattermostConfig struct {
	Bridge    BridgeConfig
	Formatter FormatterConfig

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

	JoinDM bool

	ShowMentions bool

	DisableDefaultMentions bool
	DisableShowOwnModified bool

	DisableAutoView    bool
	LastViewedSaveFile string
}

type SlackConfig struct {
	Bridge    BridgeConfig
	Formatter FormatterConfig

	DenyUsers      []string
	JoinDM         bool
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
	mmBridge := BridgeConfig{
		Restrict: append([]string(nil), c.v.GetStringSlice("mattermost.Restrict")...),

		JoinOnly:    c.v.GetStringSlice("mattermost.JoinOnly"),
		JoinExclude: c.v.GetStringSlice("mattermost.JoinExclude"),
		JoinInclude: c.v.GetStringSlice("mattermost.JoinInclude"),

		ShowJoinPart:   c.v.GetBool("mattermost.ShowJoinPart"),
		ShowOnlyJoined: c.v.GetBool("mattermost.ShowOnlyJoined"),
		PartFake:       c.v.GetBool("mattermost.PartFake"),

		PrefixContext:    c.v.GetBool("mattermost.PrefixContext"),
		SuffixContext:    c.v.GetBool("mattermost.SuffixContext"),
		ThreadContext:    c.v.GetString("mattermost.ThreadContext"),
		ShowContextMulti: c.v.GetBool("mattermost.ShowContextMulti"),
	}
	slBridge := BridgeConfig{
		Restrict: append([]string(nil), c.v.GetStringSlice("slack.Restrict")...),

		JoinOnly:    c.v.GetStringSlice("slack.JoinOnly"),
		JoinExclude: c.v.GetStringSlice("slack.JoinExclude"),
		JoinInclude: c.v.GetStringSlice("slack.JoinInclude"),

		ShowJoinPart:   c.v.GetBool("slack.ShowJoinPart"),
		ShowOnlyJoined: c.v.GetBool("slack.ShowOnlyJoined"),
		PartFake:       c.v.GetBool("slack.PartFake"),

		PrefixContext:    c.v.GetBool("slack.PrefixContext"),
		SuffixContext:    c.v.GetBool("slack.SuffixContext"),
		ThreadContext:    c.v.GetString("slack.ThreadContext"),
		ShowContextMulti: c.v.GetBool("slack.ShowContextMulti"),
	}
	mdBridge := BridgeConfig{
		Restrict: append([]string(nil), c.v.GetStringSlice("mastodon.Restrict")...),

		JoinOnly:    c.v.GetStringSlice("mastodon.JoinOnly"),
		JoinExclude: c.v.GetStringSlice("mastodon.JoinExclude"),
		JoinInclude: c.v.GetStringSlice("mastodon.JoinInclude"),

		ShowJoinPart:   c.v.GetBool("mastodon.ShowJoinPart"),
		ShowOnlyJoined: c.v.GetBool("mastodon.ShowOnlyJoined"),
		PartFake:       c.v.GetBool("mastodon.PartFake"),

		PrefixContext:    c.v.GetBool("mastodon.PrefixContext"),
		SuffixContext:    c.v.GetBool("mastodon.SuffixContext"),
		ThreadContext:    c.v.GetString("mastodon.ThreadContext"),
		ShowContextMulti: c.v.GetBool("mastodon.ShowContextMulti"),
	}

	mmFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("mattermost.DisableEmoji"),

		DisableMarkdown:           c.v.GetBool("mattermost.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("mattermost.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    c.v.GetString("mattermost.MarkdownBlockQuoteChar"),
		MarkdownInlineCode:        c.v.GetString("mattermost.MarkdownInlineCode"),

		SyntaxHighlighting:     c.v.GetString("mattermost.SyntaxHighlighting"),
		DisableCodeBlockPrefix: c.v.GetBool("mattermost.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        c.v.GetString("mattermost.CodeBlockPrefix"),

		Unicode: c.v.GetBool("mattermost.Unicode"),
	}
	slFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("slack.DisableEmoji"),

		DisableMarkdown:           c.v.GetBool("slack.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("slack.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    c.v.GetString("slack.MarkdownBlockQuoteChar"),
		MarkdownInlineCode:        c.v.GetString("slack.MarkdownInlineCode"),

		SyntaxHighlighting:     c.v.GetString("slack.SyntaxHighlighting"),
		DisableCodeBlockPrefix: c.v.GetBool("slack.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        c.v.GetString("slack.CodeBlockPrefix"),

		Unicode: c.v.GetBool("slack.Unicode"),
	}
	mdFormatter := FormatterConfig{
		DisableEmoji: c.v.GetBool("mastodon.DisableEmoji"),

		DisableMarkdown:           c.v.GetBool("mastodon.DisableMarkdown"),
		DisableMarkdownBlockQuote: c.v.GetBool("mastodon.DisableMarkdownBlockQuote"),
		MarkdownBlockQuoteChar:    c.v.GetString("mastodon.MarkdownBlockQuoteChar"),
		MarkdownInlineCode:        c.v.GetString("mastodon.MarkdownInlineCode"),

		SyntaxHighlighting:     c.v.GetString("mastodon.SyntaxHighlighting"),
		DisableCodeBlockPrefix: c.v.GetBool("mastodon.DisableCodeBlockPrefix"),
		CodeBlockPrefix:        c.v.GetString("mastodon.CodeBlockPrefix"),

		Unicode: c.v.GetBool("mastodon.Unicode"),
	}

	return &RuntimeConfig{
		GlobalConfig: GlobalConfig{
			Bind: c.v.GetString("bind"),

			Debug: c.v.GetBool("debug"),
			Trace: c.v.GetBool("trace"),
			Gops:  c.v.GetBool("gops"),

			TLSBind: c.v.GetString("tlsbind"),
			TLSDir:  c.v.GetString("tlsdir"),
			TLSKey:  c.v.GetString("tlskey"),
			TLSCert: c.v.GetString("tlscert"),

			HandshakeTimeout: c.v.GetInt("HandshakeTimeout"),
			ClientTimeout:    c.v.GetInt("ClientTimeout"),

			PasteBufferTimeout: c.v.GetInt("PasteBufferTimeout"),
		},

		Mattermost: MattermostConfig{
			Bridge:    mmBridge,
			Formatter: mmFormatter,

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

			JoinDM: c.v.GetBool("mattermost.JoinDM"),

			ShowMentions: c.v.GetBool("mattermost.ShowMentions"),

			DisableDefaultMentions: c.v.GetBool("mattermost.DisableDefaultMentions"),
			DisableShowOwnModified: c.v.GetBool("mattermost.DisableShowOwnModified"),

			DisableAutoView:    c.v.GetBool("mattermost.DisableAutoView"),
			LastViewedSaveFile: c.v.GetString("mattermost.LastViewedSaveFile"),
		},

		Slack: SlackConfig{
			Bridge:    slBridge,
			Formatter: slFormatter,

			DenyUsers:      append([]string(nil), c.v.GetStringSlice("slack.DenyUsers")...),
			JoinDM:         c.v.GetBool("slack.JoinDM"),
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

func (c *Config) Current() *RuntimeConfig {
	return c.current.Load()
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

	if err := c.reload(); err != nil {
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

func (c *Config) reload() error {
	if err := c.v.ReadInConfig(); err != nil {
		return err
	}

	if err := c.publishRuntimeConfig(); err != nil {
		return err
	}

	if logger != nil {
		logger.Info("configuration reloaded")
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

func validate(runtimeCfg *RuntimeConfig) error {
	return nil
}

func (c *Config) watch() {
	// reload config on file changes
	if runtime.GOOS == "illumos" {
		return
	}

	c.v.OnConfigChange(func(_ fsnotify.Event) {
		if err := c.reload(); err != nil && logger != nil {
			logger.WithError(err).Error("config reload failed")
		}
	})

	c.v.WatchConfig()
}
