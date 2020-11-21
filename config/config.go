package config

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var Logger *logrus.Entry

func LoadConfig(cfgfile string) (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigFile(cfgfile)

	v.SetEnvPrefix("matterircd")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	// use environment variables
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file %s", err)
	}

	// reload config on file changes
	if runtime.GOOS != "illumos" {
		v.WatchConfig()
	}

	return v, nil
}
