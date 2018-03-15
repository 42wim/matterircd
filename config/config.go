package config

import (
	"github.com/BurntSushi/toml"
	"github.com/sirupsen/logrus"
)

var Logger *logrus.Entry

type Config struct {
	Debug         bool
	Bind          string
	BindInterface string
	BindPort      int
	TLSBind       string
	TLSDir        string
	Insecure      bool
	SkipTLSVerify bool
	DefaultServer string
	DefaultTeam   string
	Restrict      string
	JoinExclude   []string
	JoinInclude   []string
	PartFake      bool
}

func LoadConfig(cfgfile string, defaultcfg Config) *Config {
	if _, err := toml.DecodeFile(cfgfile, &defaultcfg); err != nil {
		Logger.Fatalf("Error loading config file %s: %s", cfgfile, err)
	}
	Logger.Infof("Loaded config from %s", cfgfile)
	return &defaultcfg
}
