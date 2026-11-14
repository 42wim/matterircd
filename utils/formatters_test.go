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
