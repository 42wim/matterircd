package utils

import "testing"

func TestEmojiFromAliasPrecedence(t *testing.T) {
	tests := []struct {
		alias string
		want  string
	}{
		{"eyes", "👀"},
		{":eyes:", "👀"},
		{"star_struck", "🤩"},
		{":star_struck:", "🤩"},
	}

	for _, tc := range tests {
		got, ok := EmojiFromAlias(tc.alias, nil)
		if !ok {
			t.Fatalf("EmojiFromAlias(%q) returned ok=false, want %q", tc.alias, tc.want)
		}

		if got != tc.want {
			t.Errorf("EmojiFromAlias(%q) = %q, want %q", tc.alias, got, tc.want)
		}
	}
}

func TestEmojiReplaceAliasesEyes(t *testing.T) {
	got := EmojiReplaceAliases("take a look :eyes:", nil)
	want := "take a look 👀"
	if got != want {
		t.Errorf("EmojiReplaceAliases() = %q, want %q", got, want)
	}
}
