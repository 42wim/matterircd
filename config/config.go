package config

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var Logger *logrus.Entry

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

type MattermostConfig struct {
	DefaultServer string
	DefaultTeam   string

	Insecure bool

	DisableMarkdown bool
	DisableEmoji    bool

	Unicode bool

	PrefixContext bool
	SuffixContext bool

	ShowMentions bool
}

type SlackConfig struct {
	Restrict []string
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

			DisableMarkdown: c.v.GetBool("mattermost.DisableMarkdown"),
			DisableEmoji:    c.v.GetBool("mattermost.DisableEmoji"),
			Unicode:         c.v.GetBool("mattermost.Unicode"),

			PrefixContext: c.v.GetBool("mattermost.PrefixContext"),
			SuffixContext: c.v.GetBool("mattermost.SuffixContext"),

			ShowMentions: c.v.GetBool("mattermost.ShowMentions"),
		},

		Slack: SlackConfig{
			Restrict: append([]string(nil),
				c.v.GetStringSlice("slack.Restrict")...),
		},

		Mastodon: MastodonConfig{
			Server: c.v.GetString("mastodon.server"),

			ClientID:     c.v.GetString("mastodon.clientid"),
			ClientSecret: c.v.GetString("mastodon.clientsecret"),
			AccessToken:  c.v.GetString("mastodon.accesstoken"),
		},
	}
}

func (c *Config) reload() error {
	if err := c.v.ReadInConfig(); err != nil {
		return err
	}

	runtimeCfg := c.buildRuntimeCfg()

	if err := validate(runtimeCfg); err != nil {
		return err
	}

	c.current.Store(runtimeCfg)

	if Logger != nil {
		Logger.Info("configuration reloaded")
	}

	return nil
}

func validate(runtimeCfg *RuntimeConfig) error {
	return nil
}

func (c *Config) Current() *RuntimeConfig {
	return c.current.Load()
}

func Load(cfgfile string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(cfgfile)

	v.SetEnvPrefix("matterircd")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	// use environment variables
	v.AutomaticEnv()

	c := &Config{
		v: v,
	}

	if err := c.reload(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	// reload config on file changes
	if runtime.GOOS != "illumos" {
		v.OnConfigChange(func(e fsnotify.Event) {
			if err := c.reload(); err != nil {
				if Logger != nil {
					Logger.Errorf("config reload failed: %v", err)
				}
			}
		})

		v.WatchConfig()
	}

	return c, nil
}
