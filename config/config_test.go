package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

// newReloadableConfig returns a configuration that has a config file on disk, so Reload works.
func newReloadableConfig(t *testing.T) *Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "matterircd.toml")

	// Loading a config file that doesn't exist yields the defaults.
	cfg, err := Load(path, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatalf("unable to load default config: %v", err)
	}

	err = os.WriteFile(path, []byte("Debug = true\n"), 0o600)
	if err != nil {
		t.Fatalf("unable to write config file: %v", err)
	}

	return cfg
}

func mustReload(t *testing.T, cfg *Config) {
	t.Helper()

	err := cfg.Reload()
	if err != nil {
		t.Fatalf("unable to reload config: %v", err)
	}
}

func TestReloadHooks(t *testing.T) {
	cfg := newReloadableConfig(t)

	var first, second int

	cfg.RegisterReloadHook(func(rc *RuntimeConfig) {
		first++

		assert.True(t, rc.Debug)
	})
	unregisterSecond := cfg.RegisterReloadHook(func(*RuntimeConfig) { second++ })

	mustReload(t, cfg)
	assert.Equal(t, 1, first)
	assert.Equal(t, 1, second)

	// An unregistered hook is gone, the others keep running. Unregistering twice is harmless.
	unregisterSecond()
	unregisterSecond()

	mustReload(t, cfg)
	assert.Equal(t, 2, first)
	assert.Equal(t, 1, second)
}

// Every login registers a hook. Without unregistering them the reload hooks (and everything they
// reference) of all the sessions that ever existed would stay around forever.
func TestReloadHooksDontAccumulate(t *testing.T) {
	cfg := newReloadableConfig(t)

	for range 100 {
		unregister := cfg.RegisterReloadHook(func(*RuntimeConfig) {})
		unregister()
	}

	cfg.hooksMu.RLock()
	defer cfg.hooksMu.RUnlock()

	assert.Empty(t, cfg.reloadHooks)
}

// A hook can unregister itself (or others) while the reload is running it.
func TestReloadHookUnregistersDuringReload(t *testing.T) {
	cfg := newReloadableConfig(t)

	var (
		calls      int
		unregister func()
	)

	unregister = cfg.RegisterReloadHook(func(*RuntimeConfig) {
		calls++

		unregister()
	})

	mustReload(t, cfg)
	mustReload(t, cfg)
	assert.Equal(t, 1, calls)
}

// Hooks are (un)registered by logins while a reload is running.
func TestReloadHooksConcurrent(t *testing.T) {
	cfg := newReloadableConfig(t)

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			for range 50 {
				unregister := cfg.RegisterReloadHook(func(*RuntimeConfig) {})
				unregister()
			}
		})
	}

	// reloads are never run concurrently with each other
	for range 5 {
		mustReload(t, cfg)
	}

	wg.Wait()

	cfg.hooksMu.RLock()
	defer cfg.hooksMu.RUnlock()

	assert.Empty(t, cfg.reloadHooks)
}
