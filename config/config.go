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

	Debug   bool
	Trace   bool
	Gops    bool

	TLSBind string
	TLSDir  string
	TLSKey  string
	TLSCert string

	HandshakeTimeout int
	ClientTimeout    int

	PasteBufferTimeout int
}

type MattermostConfig struct {
	DefaultServer string
	DefaultTeam   string

	Insecure bool

	LastViewedSaveFile string

	DisableMarkdown bool
	DisableEmoji    bool

	Unicode bool

	PrefixContext bool
	SuffixContext bool

	ShowMentions bool
}

type SlackConfig struct {
	DenyUsers      []string
	JoinDM         bool
	Restrict       []string
	UseDisplayName bool
	PreferNickname bool
	JoinOnly       []string
	JoinExclude    []string
	JoinInclude    []string
	ShowOnlyJoined bool
	PrefixContext  bool
}

type MastodonConfig struct {
	Server string

	ClientID     string
	ClientSecret string
	AccessToken  string
}

func (c *Config) buildRuntimeCfg() *RuntimeConfig {
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
			DefaultServer: c.v.GetString("mattermost.DefaultServer"),
			DefaultTeam:   c.v.GetString("mattermost.DefaultTeam"),

			Insecure: c.v.GetBool("mattermost.Insecure"),

			LastViewedSaveFile: c.v.GetString("mattermost.LastViewedSaveFile"),

			DisableMarkdown: c.v.GetBool("mattermost.DisableMarkdown"),
			DisableEmoji:    c.v.GetBool("mattermost.DisableEmoji"),
			Unicode:         c.v.GetBool("mattermost.Unicode"),

			PrefixContext: c.v.GetBool("mattermost.PrefixContext"),
			SuffixContext: c.v.GetBool("mattermost.SuffixContext"),

			ShowMentions: c.v.GetBool("mattermost.ShowMentions"),
		},

		Slack: SlackConfig{
			DenyUsers:      append([]string(nil), c.v.GetStringSlice("slack.DenyUsers")...),
			JoinDM:         c.v.GetBool("slack.JoinDM"),
			Restrict:       append([]string(nil), c.v.GetStringSlice("slack.Restrict")...),
			UseDisplayName: c.v.GetBool("slack.UseDisplayName"),
			PreferNickname: c.v.GetBool("slack.PreferNickname"),

			JoinOnly:    append([]string(nil), c.v.GetStringSlice("slack.JoinOnly")...),
			JoinExclude: append([]string(nil), c.v.GetStringSlice("slack.JoinExclude")...),
			JoinInclude: append([]string(nil), c.v.GetStringSlice("slack.JoinInclude")...),

			ShowOnlyJoined: c.v.GetBool("slack.ShowOnlyJoined"),
			PrefixContext:  c.v.GetBool("slack.PrefixContext"),
		},

		Mastodon: MastodonConfig{
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
