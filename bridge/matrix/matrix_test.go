package matrix

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"

	"maunium.net/go/mautrix/event"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

func loadTestConfig(t *testing.T, tomlContent string) *config.Config {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "matterircd.toml")

	err := os.WriteFile(cfgPath, []byte(tomlContent), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

func TestMatrixOnReconnect(t *testing.T) {
	tests := []struct {
		name     string
		toml     string
		ready    bool
		downtime time.Duration
		wantCall bool
	}{
		{name: "skips when not ready", ready: false, downtime: 30 * time.Minute},
		{name: "skips brief disconnect below default 15m", ready: true, downtime: 10 * time.Minute},
		{name: "triggers at default 15m threshold", ready: true, downtime: 15 * time.Minute, wantCall: true},
		{name: "skips below 5m HeavySyncThreshold", toml: "[matrix]\nHeavySyncThreshold = \"5m\"\n", ready: true, downtime: 4 * time.Minute},
		{name: "triggers at 5m HeavySyncThreshold", toml: "[matrix]\nHeavySyncThreshold = \"5m\"\n", ready: true, downtime: 5 * time.Minute, wantCall: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadTestConfig(t, tc.toml)
			if tc.toml != "" {
				assert.Equal(t, 5*time.Minute, cfg.Matrix().HeavySyncThreshold)
			}

			called := make(chan struct{}, 1)
			m := &Matrix{cfg: cfg, ctx: context.Background(), onConnect: func() { called <- struct{}{} }}
			m.ready.Store(tc.ready)
			m.OnReconnect(tc.downtime)

			if tc.wantCall {
				select {
				case <-called:
				case <-time.After(time.Second):
					t.Fatal("expected onConnect to be called")
				}

				return
			}

			select {
			case <-called:
				t.Fatal("onConnect should not be called")
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

//nolint:goconst
func TestExtractEventBody(t *testing.T) {
	codeBlock := "```bash\nTesting\nTesting again\nTesting bash code block...\n```" //nolint:dupword

	ev := &event.Event{
		Content: event.Content{
			Raw: map[string]any{
				"body": "> <@alice:example.org> parent\n\n" + codeBlock,
			},
		},
	}
	assert.Equal(t, codeBlock, extractEventBody(ev))

	editedEv := &event.Event{
		Content: event.Content{
			Raw: map[string]any{
				"body": "* original",
				"m.new_content": map[string]any{
					"body": codeBlock,
				},
			},
		},
	}
	assert.Equal(t, codeBlock, extractEventBody(editedEv))
	assert.Empty(t, extractEventBody(nil))
}

func TestMatrixPinFormatting(t *testing.T) {
	cfgUnicode := loadTestConfig(t, "[matrix]\nUnicode = true\n")
	mUnicode := &Matrix{cfg: cfgUnicode}
	assert.Equal(t, " \x1d(📌pinned)\x1d", mUnicode.formatSuffix(utils.SuffixPinned))
	assert.Equal(t, " \x1d(📌unpinned)\x1d", mUnicode.formatSuffix(utils.SuffixUnpinned))

	cfgASCII := loadTestConfig(t, "[matrix]\nUnicode = false\n")
	mASCII := &Matrix{cfg: cfgASCII}
	assert.Equal(t, " \x1d(pinned)\x1d", mASCII.formatSuffix(utils.SuffixPinned))
	assert.Equal(t, " \x1d(unpinned)\x1d", mASCII.formatSuffix(utils.SuffixUnpinned))
}
