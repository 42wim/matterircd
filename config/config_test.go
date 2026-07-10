package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDoesNotAutoReloadChangedFile(t *testing.T) {
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

	if v.GetBool("debug") {
		t.Fatal("expected config value to stay unchanged without automatic reload")
	}
}
