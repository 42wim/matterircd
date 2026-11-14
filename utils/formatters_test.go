package utils

import (
	"slices"
	"testing"
)

func collectProcessedLines(text string, opts ProcessMessageOpts) []string {
	var lines []string

	ProcessMessageText(text, opts, func(line string) {
		lines = append(lines, line)
	})

	return lines
}

//nolint:dupword,goconst
func TestProcessMessageTextCodeBlockInlineClose(t *testing.T) {
	opts := ProcessMessageOpts{
		CodeBlockPrefix:    "🮇 ",
		SyntaxHighlighting: "terminal256:pygments",
	}

	tests := []struct {
		name      string
		input     string
		wantLines []string
	}{
		{
			name:  "standard closing fence with edited suffix",
			input: "```bash\nTesting\nTesting again\nTesting bash code block\n``` \x1d(✏️edited)\x1d",
			wantLines: []string{
				"🮇 \x16bash\x16",
				"🮇 Testing",
				"🮇 Testing again",
				"🮇 Testing bash code block",
				"\x1d(✏️edited)\x1d",
			},
		},
		{
			name:  "inline closing fence with edited suffix",
			input: "```bash\nTesting\nTesting again\nTesting bash code block``` \x1d(✏️edited)\x1d",
			wantLines: []string{
				"🮇 \x16bash\x16",
				"🮇 Testing",
				"🮇 Testing again",
				"🮇 Testing bash code block",
				"\x1d(✏️edited)\x1d",
			},
		},
		{
			name:  "inline closing fence without trailing text",
			input: "```bash\nTesting bash code block```",
			wantLines: []string{
				"🮇 \x16bash\x16",
				"🮇 Testing bash code block",
			},
		},
		{
			name:  "indented code block with inline closing fence",
			input: "  ```bash\n  echo hi``` \x1d(edited)\x1d",
			wantLines: []string{
				"  🮇 \x16bash\x16",
				"  🮇 \x1b[38;5;2mecho\x1b[0m hi",
				"\x1d(edited)\x1d",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collectProcessedLines(tc.input, opts)
			if !slices.Equal(got, tc.wantLines) {
				t.Errorf("got: %q\nwant: %q", got, tc.wantLines)
			}
		})
	}
}

func TestProcessMessageTextCodeBlockMidLineBackticks(t *testing.T) {
	opts := ProcessMessageOpts{
		CodeBlockPrefix:    "🮇 ",
		SyntaxHighlighting: "terminal256:pygments",
	}

	tests := []struct {
		name      string
		input     string
		wantLines []string
	}{
		{
			name:  "code block containing inline backticks mid-line is not prematurely closed",
			input: "```bash\necho \"```hello```\"\n```",
			wantLines: []string{
				"🮇 \x16bash\x16",
				"🮇 \x1b[38;5;2mecho\x1b[0m \x1b[38;5;124m\"```hello```\"\x1b[0m",
			},
		},
		{
			name:  "code block containing backticks with trailing words is not prematurely closed",
			input: "```markdown\nUse ```code``` here\n```",
			wantLines: []string{
				"🮇 \x16markdown\x16",
				"🮇 Use ``\x1b[38;5;124m`code`\x1b[0m`` here",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collectProcessedLines(tc.input, opts)
			if !slices.Equal(got, tc.wantLines) {
				t.Errorf("got: %q\nwant: %q", got, tc.wantLines)
			}
		})
	}
}

//nolint:goconst
func TestFormatActionSuffix(t *testing.T) {
	tests := []struct {
		kind    ActionSuffixKind
		unicode bool
		want    string
	}{
		{SuffixEdited, true, " \x1d(✏️edited)\x1d"},
		{SuffixEdited, false, " \x1d(edited)\x1d"},
		{SuffixPinned, true, " \x1d(📌pinned)\x1d"},
		{SuffixPinned, false, " \x1d(pinned)\x1d"},
		{SuffixUnpinned, true, " \x1d(📌unpinned)\x1d"},
		{SuffixUnpinned, false, " \x1d(unpinned)\x1d"},
		{SuffixUpdated, true, " \x1d(🔄updated)\x1d"},
		{SuffixUpdated, false, " \x1d(updated)\x1d"},
		{SuffixDeleted, true, " \x1d(🗑️deleted)\x1d"},
		{SuffixDeleted, false, " \x1d(deleted)\x1d"},
		{SuffixEncrypted, true, " \x1d(🔒encrypted)\x1d"},
		{SuffixEncrypted, false, " \x1d(encrypted)\x1d"},
		{ActionSuffixKind("unknown"), true, ""},
	}

	for _, tc := range tests {
		if got := FormatActionSuffix(tc.kind, tc.unicode); got != tc.want {
			t.Errorf("FormatActionSuffix(%q, %v) = %q, want %q", tc.kind, tc.unicode, got, tc.want)
		}
	}
}

func TestAppendMessageSuffix(t *testing.T) {
	tests := []struct {
		name   string
		msg    string
		suffix string
		want   string
	}{
		{name: "plain message", msg: "hello\n\n", suffix: " \x1d(📌pinned)\x1d", want: "hello \x1d(📌pinned)\x1d"},
		{name: "empty message", msg: "", suffix: " \x1d(📌pinned)\x1d", want: "\x1d(📌pinned)\x1d"},
		{name: "single-line action", msg: "\x01ACTION waves\x01", suffix: " \x1d(📌pinned)\x1d", want: "\x01ACTION waves \x1d(📌pinned)\x1d\x01"},
		{name: "empty suffix", msg: "hello\r\n", suffix: "", want: "hello"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AppendMessageSuffix(tc.msg, tc.suffix); got != tc.want {
				t.Errorf("AppendMessageSuffix(%q, %q) = %q, want %q", tc.msg, tc.suffix, got, tc.want)
			}
		})
	}
}

func TestProcessMessageTextQuotedCodeBlock(t *testing.T) {
	opts := ProcessMessageOpts{
		BlockquoteChar:     "\x0310🮇\x0f\u00a0",
		CodeBlockPrefix:    "🮇 ",
		SyntaxHighlighting: "terminal256:pygments",
	}

	input := "https://example.com\n> ```bash\n> echo \"hello\"\n> ``` \x1d(📌pinned)\x1d"
	wantLines := []string{
		"https://example.com",
		"\x0310🮇\x0f\u00a0🮇 \x16bash\x16",
		"\x0310🮇\x0f\u00a0🮇 \x1b[38;5;2mecho\x1b[0m \x1b[38;5;124m\"hello\"\x1b[0m",
		"\x1d(📌pinned)\x1d",
	}

	got := collectProcessedLines(input, opts)
	if !slices.Equal(got, wantLines) {
		t.Errorf("got: %q\nwant: %q", got, wantLines)
	}

	// mm-go-irckit pre-replaces the leading '>' on line 1 with BlockquoteChar before ProcessMessageText.
	preformattedInput := "\x0310🮇\x0f\u00a0 ```bash\n> echo \"hello\"\n> ```"
	wantPreformatted := []string{
		"\x0310🮇\x0f\u00a0🮇 \x16bash\x16",
		"\x0310🮇\x0f\u00a0🮇 \x1b[38;5;2mecho\x1b[0m \x1b[38;5;124m\"hello\"\x1b[0m",
	}

	gotPre := collectProcessedLines(preformattedInput, opts)
	if !slices.Equal(gotPre, wantPreformatted) {
		t.Errorf("preformatted got: %q\nwant: %q", gotPre, wantPreformatted)
	}
}
