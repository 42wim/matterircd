package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var Logger *logrus.Entry

type Viper struct {
	mu sync.RWMutex
	v  *viper.Viper
}

func New() *Viper {
	return &Viper{v: viper.New()}
}

func LoadConfig(cfgfile string) (*Viper, error) {
	v := New()
	v.SetConfigFile(cfgfile)

	v.SetEnvPrefix("matterircd")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	// use environment variables
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file %s", err)
	}

	if err := v.WatchConfig(); err != nil {
		return nil, fmt.Errorf("error watching config file %s", err)
	}

	return v, nil
}

func (v *Viper) SetConfigFile(cfgfile string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.v.SetConfigFile(cfgfile)
}

func (v *Viper) SetEnvPrefix(prefix string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.v.SetEnvPrefix(prefix)
}

func (v *Viper) SetEnvKeyReplacer(replacer *strings.Replacer) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.v.SetEnvKeyReplacer(replacer)
}

func (v *Viper) AutomaticEnv() {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.v.AutomaticEnv()
}

func (v *Viper) ReadInConfig() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.v.ReadInConfig()
}

func (v *Viper) BindPFlags(flags *pflag.FlagSet) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.v.BindPFlags(flags)
}

func (v *Viper) GetBool(key string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.v.GetBool(key)
}

func (v *Viper) GetString(key string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.v.GetString(key)
}

func (v *Viper) GetInt(key string) int {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.v.GetInt(key)
}

func (v *Viper) GetStringSlice(key string) []string {
	v.mu.RLock()

	values := v.v.GetStringSlice(key)
	values = append([]string(nil), values...)

	v.mu.RUnlock()

	return values
}

func (v *Viper) Set(key string, value any) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.v.Set(key, value)
}

func (v *Viper) WatchConfig() error {
	filename, err := v.getConfigFile()
	if err != nil {
		return err
	}

	configDir := filepath.Dir(filename)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	if err := watcher.Add(configDir); err != nil {
		_ = watcher.Close()
		return err
	}

	go v.watchConfig(watcher, filename)

	return nil
}

func (v *Viper) watchConfig(watcher *fsnotify.Watcher, filename string) {
	defer watcher.Close()

	configFile := filepath.Clean(filename)
	realConfigFile, _ := filepath.EvalSymlinks(filename)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			currentConfigFile, _ := filepath.EvalSymlinks(filename)
			isDirectFileChange := filepath.Clean(event.Name) == configFile &&
				(event.Has(fsnotify.Write) || event.Has(fsnotify.Create))
			isSymlinkChange := currentConfigFile != "" && currentConfigFile != realConfigFile
			if isDirectFileChange || isSymlinkChange {
				realConfigFile = currentConfigFile

				if err := v.ReadInConfig(); err != nil {
					logError("read config file", err)
				}

				continue
			}

			if filepath.Clean(event.Name) == configFile && event.Has(fsnotify.Remove) {
				return
			}
		case err, ok := <-watcher.Errors:
			if ok {
				logError("watcher error", err)
			}

			return
		}
	}
}

func (v *Viper) getConfigFile() (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	filename := v.v.ConfigFileUsed()
	if filename == "" {
		return "", fmt.Errorf("config file path is empty")
	}

	return filename, nil
}

func logError(message string, err error) {
	if err == nil {
		return
	}

	if Logger != nil {
		Logger.Errorf("%s: %v", message, err)
		return
	}

	_, _ = fmt.Fprintf(os.Stderr, "%s: %v\n", message, err)
}
