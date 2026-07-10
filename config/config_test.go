package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigAutoReloadsChangedFile(t *testing.T) {
	t.Setenv("MATTERIRCD_DEBUG", "")

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "matterircd.toml")
	if err := os.WriteFile(cfgFile, []byte("debug = false\n"), 0o600); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	v, err := LoadConfig(cfgFile)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if v.GetBool("debug") {
		t.Fatal("expected initial debug value to be false")
	}

	if err := os.WriteFile(cfgFile, []byte("debug = true\n"), 0o600); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v.GetBool("debug") {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("expected config value to reload after file change")
}
