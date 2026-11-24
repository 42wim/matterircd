package utils

import (
	"slices"
	"testing"
	"time"
)

func collectProcessedLines(text string, opts ProcessMessageOpts) []string {
	var lines []string

	ProcessMessageText(text, opts, func(line string) {
		lines = append(lines, line)
	})

	return lines
}

//nolint:dupword,funlen,goconst
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
			name:  "standard closing fence with reply suffix",
			input: "```\nhello\n``` (re @user: previous msg)",
			wantLines: []string{
				"🮇 hello",
				"(re @user: previous msg)",
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

//nolint:funlen
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
		{
			name: "diff code block containing removed/added markdown fences is not prematurely closed",
			input: "```diff\n" +
				"-  ```\n" +
				"-  command=\"rsync\"\n" +
				"-  ```\n" +
				"+```\n" +
				"+echo hi\n" +
				"+```\n" +
				"```",
			wantLines: []string{
				"🮇 \x16diff\x16",
				"🮇 \x1b[38;5;124m-  ```\x1b[0m",
				"🮇 \x1b[38;5;124m-  command=\"rsync\"\x1b[0m",
				"🮇 \x1b[38;5;124m-  ```\x1b[0m",
				"🮇 \x1b[38;5;34m+```\x1b[0m",
				"🮇 \x1b[38;5;34m+echo hi\x1b[0m",
				"🮇 \x1b[38;5;34m+```\x1b[0m",
			},
		},
		{
			name: "four-backtick fence containing triple-backtick lines is not prematurely closed",
			input: "````markdown\n" +
				"```bash\n" +
				"echo hi\n" +
				"```\n" +
				"````",
			wantLines: []string{
				"🮇 \x16markdown\x16",
				"🮇 \x1b[38;5;124m```bash\x1b[0m",
				"🮇 \x1b[38;5;2mecho\x1b[0m hi",
				"🮇 \x1b[38;5;124m```\x1b[0m",
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

func TestFormatLastOnline(t *testing.T) {
	now := time.Now().Unix()

	if got := FormatLastOnline(0); got != "" {
		t.Errorf("expected empty string for 0, got %q", got)
	}

	if got := FormatLastOnline(now - 10); got != "Last online just now" {
		t.Errorf("expected 'Last online just now', got %q", got)
	}

	if got := FormatLastOnline(now-10, true); got != "Last activity just now" {
		t.Errorf("expected 'Last activity just now', got %q", got)
	}

	if got := FormatLastOnline(now - 60); got != "Last online 1 min. ago" {
		t.Errorf("expected 'Last online 1 min. ago', got %q", got)
	}

	if got := FormatLastOnline(now - 3600); got != "Last online 1 hr. ago" {
		t.Errorf("expected 'Last online 1 hr. ago', got %q", got)
	}

	if got := FormatLastOnline(now - 86400); got != "Last online 1 day ago" {
		t.Errorf("expected 'Last online 1 day ago', got %q", got)
	}
}

//nolint:goconst
func TestMarkdown2ircHeadings(t *testing.T) {
	codeStart := "\x0f`\x11\x02\x030,14"
	codeEnd := "\x11\x0f`"

	tests := []struct {
		name    string
		input   string
		bq      string
		unicode bool
		want    string
	}{
		{name: "h1 unicode", input: "# Heading 1", unicode: true, want: "═══ \x02Heading 1\x02 ═══"},
		{name: "h2 unicode", input: "## Heading 2", unicode: true, want: "━━ \x02Heading 2\x02 ━━"},
		{name: "h3 unicode", input: "### Heading 3", unicode: true, want: "── Heading 3 ──"},
		{name: "h4 unicode", input: "#### Heading 4", unicode: true, want: "─ Heading 4 ─"},
		{name: "h5 unicode", input: "##### Heading 5", unicode: true, want: "┄ Heading 5 ┄"},
		{name: "h6 unicode", input: "###### Heading 6", unicode: true, want: "· Heading 6 ·"},
		{name: "h1 ascii", input: "# Heading 1", want: "=== \x02Heading 1\x02 ==="},
		{name: "h2 ascii", input: "## Heading 2", want: "== \x02Heading 2\x02 =="},
		{name: "h3 ascii", input: "### Heading 3", want: "-- Heading 3 --"},
		{name: "h4 ascii", input: "#### Heading 4", want: "- Heading 4 -"},
		{name: "h5 ascii", input: "##### Heading 5", want: ".. Heading 5 .."},
		{name: "h6 ascii", input: "###### Heading 6", want: ". Heading 6 ."},
		{name: "seven hashes not heading", input: "####### Not a heading", unicode: true, want: "####### Not a heading"},
		{name: "hashtag not heading", input: "#channel", unicode: true, want: "#channel"},
		{name: "empty heading ignored", input: "#   ", unicode: true, want: "#   "},
		{name: "up to 3 leading spaces", input: "   ## Indented", unicode: true, want: "━━ \x02Indented\x02 ━━"},
		{name: "4 leading spaces not heading", input: "    # Code indent", unicode: true, want: "    # Code indent"},
		{name: "closing hashes stripped", input: "## Title ##", unicode: true, want: "━━ \x02Title\x02 ━━"},
		{name: "non-space trailing hash kept", input: "## C#", unicode: true, want: "━━ \x02C#\x02 ━━"},
		{name: "bold inside h1 does not double toggle", input: "# **Bold** and *italic*", unicode: true, want: "═══ \x02Bold and \x1ditalic\x1d\x02 ═══"},
		{name: "bold inside h3 renders bold", input: "### **Bold** and *italic*", unicode: true, want: "── \x02Bold\x02 and \x1ditalic\x1d ──"},
		{name: "link inside h1 keeps underline", input: "# See [docs](https://example.com)", unicode: true, want: "═══ \x02See \x1fdocs\x1f\u00a0(\x1dhttps://example.com\x1d)\x02 ═══"},
		{name: "inline code in middle of h1", input: "# Use `foo` now", unicode: true, want: "═══ \x02Use " + codeStart + "foo" + codeEnd + "\x02 now\x02 ═══"},
		{name: "inline code at end of h1", input: "# Use `foo`", unicode: true, want: "═══ \x02Use " + codeStart + "foo" + codeEnd + " ═══"},
		{name: "inline code only h1", input: "# `foo`", unicode: true, want: "═══ " + codeStart + "foo" + codeEnd + " ═══"},
		{name: "quoted h1", input: "> # Quoted", bq: "🮇 ", unicode: true, want: "🮇 ═══ \x02Quoted\x02 ═══"},
		{name: "quoted h3", input: "> ### Quoted", bq: "🮇 ", unicode: true, want: "🮇 ── Quoted ──"},
		{name: "pre-replaced quoted heading", input: "🮇 # Quoted", bq: "🮇", unicode: true, want: "🮇 ═══ \x02Quoted\x02 ═══"},
		{name: "heading with trailing edited suffix", input: "# Title \x1d(✏️edited)\x1d", unicode: true, want: "═══ \x02Title\x02 ═══ \x1d(✏️edited)\x1d"},
		{name: "quoted heading with trailing edited suffix", input: "> ## Title \x1d(✏️edited)\x1d", bq: "🮇 ", unicode: true, want: "🮇 ━━ \x02Title\x02 ━━ \x1d(✏️edited)\x1d"},
		{name: "escaped hash not heading", input: `\# Escaped`, unicode: true, want: "# Escaped"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Markdown2irc(tc.input, tc.bq, "", tc.unicode); got != tc.want {
				t.Errorf("Markdown2irc(%q, %q, \"\", %v) = %q, want %q", tc.input, tc.bq, tc.unicode, got, tc.want)
			}
		})
	}
}

//nolint:goconst
func TestMarkdown2ircUnorderedLists(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		bq      string
		unicode bool
		want    string
	}{
		{name: "dash level 0 unicode", input: "- item", unicode: true, want: "• item"},
		{name: "asterisk level 0 unicode", input: "* item", unicode: true, want: "• item"},
		{name: "plus level 0 unicode", input: "+ item", unicode: true, want: "• item"},
		{name: "nested level 1 unicode (2 spaces)", input: "  - nested 1", unicode: true, want: "  ◦ nested 1"},
		{name: "nested level 2 unicode (4 spaces)", input: "    - nested 2", unicode: true, want: "    ▪ nested 2"},
		{name: "nested level 3 unicode cycles (6 spaces)", input: "      - nested 3", unicode: true, want: "      • nested 3"},
		{name: "level 0 ascii", input: "- item", want: "* item"},
		{name: "nested level 1 ascii", input: "  - nested 1", want: "  - nested 1"},
		{name: "nested level 2 ascii", input: "    - nested 2", want: "    + nested 2"},
		{name: "nested level 3 ascii cycles", input: "      - nested 3", want: "      * nested 3"},
		{name: "tab nested level 1", input: "\t- tabbed 1", unicode: true, want: "\t◦ tabbed 1"},
		{name: "tab nested level 2", input: "\t\t- tabbed 2", unicode: true, want: "\t\t▪ tabbed 2"},
		{name: "list item with formatting", input: "* item with *italic* and **bold**", unicode: true, want: "• item with \x1ditalic\x1d and \x02bold\x02"},
		{name: "task list checkbox left untouched", input: "- [ ] todo item", unicode: true, want: "• [ ] todo item"},
		{name: "checked task list left untouched", input: "- [x] done item", unicode: true, want: "• [x] done item"},
		{name: "flag not a list", input: "--verbose", unicode: true, want: "--verbose"},
		{name: "negative number not a list", input: "-42", unicode: true, want: "-42"},
		{name: "plus number not a list", input: "+1", unicode: true, want: "+1"},
		{name: "empty marker not a list", input: "- ", unicode: true, want: "- "},
		{name: "quoted list item", input: "> - quoted", bq: "🮇 ", unicode: true, want: "🮇 • quoted"},
		{name: "quoted nested list item preserves indent", input: ">   - nested", bq: "🮇 ", unicode: true, want: "🮇   ◦ nested"},
		{name: "escaped dash not a list", input: `\- literal`, unicode: true, want: "- literal"},
		{name: "escaped asterisk not a list", input: `\* literal`, unicode: true, want: "* literal"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Markdown2irc(tc.input, tc.bq, "", tc.unicode); got != tc.want {
				t.Errorf("Markdown2irc(%q, %q, \"\", %v) = %q, want %q", tc.input, tc.bq, tc.unicode, got, tc.want)
			}
		})
	}
}

//nolint:goconst
func TestMarkdown2ircHorizontalRules(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		bq      string
		unicode bool
		want    string
	}{
		{name: "three dashes unicode", input: "---", unicode: true, want: horizontalRuleLine},
		{name: "three dashes ascii", input: "---", want: horizontalRuleLineASCII},
		{name: "many dashes", input: "-------", unicode: true, want: horizontalRuleLine},
		{name: "three asterisks", input: "***", unicode: true, want: horizontalRuleLine},
		{name: "three underscores", input: "___", unicode: true, want: horizontalRuleLine},
		{name: "spaced dashes", input: "- - -", unicode: true, want: horizontalRuleLine},
		{name: "spaced asterisks", input: "* * *", unicode: true, want: horizontalRuleLine},
		{name: "spaced underscores", input: "_ _ _", unicode: true, want: horizontalRuleLine},
		{name: "leading and trailing spaces", input: "   ---   ", unicode: true, want: horizontalRuleLine},
		{name: "4 leading spaces not HR", input: "    ---", unicode: true, want: "    ---"},
		{name: "two dashes not HR", input: "--", unicode: true, want: "--"},
		{name: "mixed markers not HR", input: "-*-", unicode: true, want: "-*-"},
		{name: "bold italic not HR", input: "***bold italic***", unicode: true, want: "\x02\x1dbold italic\x1d\x02"},
		{name: "quoted HR", input: "> ---", bq: "🮇 ", unicode: true, want: "🮇 " + horizontalRuleLine},
		{name: "HR with trailing pinned suffix", input: "--- \x1d(📌pinned)\x1d", unicode: true, want: horizontalRuleLine + " \x1d(📌pinned)\x1d"},
		{name: "quoted HR with trailing edited suffix", input: "> --- \x1d(✏️edited)\x1d", bq: "🮇 ", unicode: true, want: "🮇 " + horizontalRuleLine + " \x1d(✏️edited)\x1d"},
		{name: "escaped HR", input: `\---`, unicode: true, want: "---"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Markdown2irc(tc.input, tc.bq, "", tc.unicode); got != tc.want {
				t.Errorf("Markdown2irc(%q, %q, \"\", %v) = %q, want %q", tc.input, tc.bq, tc.unicode, got, tc.want)
			}
		})
	}
}

func TestMarkdown2ircImagesAndLinks(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"image with alt text", "![diagram](https://example.com/a.png)", "\x1f[image: diagram]\x1f\u00a0(\x1dhttps://example.com/a.png\x1d)"},
		{"image with empty alt text", "![](https://example.com/a.png)", "\x1f[image]\x1f\u00a0(\x1dhttps://example.com/a.png\x1d)"},
		{"image in sentence", "Look at ![my photo](https://example.com/_dark_/pic.png) please", "Look at \x1f[image: my photo]\x1f\u00a0(\x1dhttps://example.com/_dark_/pic.png\x1d) please"},
		{"adjacent images", "![a](https://a.test/1.png)![b](https://a.test/2.png)", "\x1f[image: a]\x1f\u00a0(\x1dhttps://a.test/1.png\x1d)\x1f[image: b]\x1f\u00a0(\x1dhttps://a.test/2.png\x1d)"},
		{"exclamation and space before regular link", "Hello! [click](https://example.com)", "Hello! \x1fclick\x1f\u00a0(\x1dhttps://example.com\x1d)"},
		{"link with bold text and underscores in URL", "[**docs**](https://example.com/_api_/v1)", "\x1f\x02docs\x02\x1f\u00a0(\x1dhttps://example.com/_api_/v1\x1d)"},
		{"link with balanced parentheses in URL", "[Go](https://en.wikipedia.org/wiki/Go_(programming_language))", "\x1fGo\x1f\u00a0(\x1dhttps://en.wikipedia.org/wiki/Go_(programming_language)\x1d)"},
		{"escaped bang before link", `\![docs](https://example.com)`, "!\x1fdocs\x1f\u00a0(\x1dhttps://example.com\x1d)"},
		{"escaped image", `\!\[alt](https://example.com/a.png)`, "![alt](https://example.com/a.png)"},
		{"image inside heading", "# ![icon](https://example.com/i.png) Title", "=== \x02\x1f[image: icon]\x1f\u00a0(\x1dhttps://example.com/i.png\x1d) Title\x02 ==="},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Markdown2irc(tc.input, "", ""); got != tc.want {
				t.Errorf("Markdown2irc(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestMarkdown2ircInlineCodeAndEscapes(t *testing.T) {
	codeStart, codeEnd := "\x0f`\x11\x02\x030,14", "\x11\x0f`"

	tests := []struct {
		name, input, inlineCode, want string
	}{
		{"bold inside inline code stays literal", "`**bold**`", "", codeStart + "**bold**" + codeEnd},
		{"italic and strike inside inline code stay literal", "`_italic_ and ~~strike~~`", "", codeStart + "_italic_ and ~~strike~~" + codeEnd},
		{"link and image inside inline code stay literal", "`[link](https://example.com)` and `![img](https://example.com/a.png)`", "", codeStart + "[link](https://example.com)" + codeEnd + " and " + codeStart + "![img](https://example.com/a.png)" + codeEnd},
		{"backslashes inside inline code stay literal", "`\\*foo\\*`", "", codeStart + `\*foo\*` + codeEnd},
		{"double backtick span containing single backtick", "``use `**bold**` here``", "", codeStart + "use `**bold**` here" + codeEnd},
		{"double backtick span with single space padding", "`` ` ``", "", codeStart + "`" + codeEnd},
		{"unclosed backtick does not suppress emphasis", "`unclosed **bold**", "", "`unclosed \x02bold\x02"},
		{"emphasis wrapping inline code", "**bold `code` bold**", "", "\x02bold " + codeStart + "code" + codeEnd + " bold\x02"},
		{"custom inline code without backtick", "`**bold**`", "\x11", "\x11**bold**\x11\x0f"},
		{"escaped italics", `\*not italic\*`, "", "*not italic*"},
		{"escaped bold", `\*\*not bold\*\*`, "", "**not bold**"},
		{"escaped underscores", `\_not italic\_`, "", "_not italic_"},
		{"escaped strikethrough", `\~\~not strike\~\~`, "", "~~not strike~~"},
		{"escaped backticks", "\\`not code\\`", "", "`not code`"},
		{"escaped link", `\[not link](https://example.com)`, "", "[not link](https://example.com)"},
		{"escaped blockquote", `\> not quote`, "", "> not quote"},
		{"escaped backslash before emphasis", `\\*italic*`, "", "\\\x1ditalic\x1d"},
		{"escaped stars inside bold", `**bold \*star\* bold**`, "", "\x02bold *star* bold\x02"},
		{"non-punctuation backslash preserved", `C:\Users\alice\file.txt`, "", `C:\Users\alice\file.txt`},
		{"trailing backslash preserved", `hello\`, "", `hello\`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Markdown2irc(tc.input, "", tc.inlineCode); got != tc.want {
				t.Errorf("Markdown2irc(%q, \"\", %q) = %q, want %q", tc.input, tc.inlineCode, got, tc.want)
			}
		})
	}
}

//nolint:funlen,goconst
func TestProcessMessageTextTables(t *testing.T) {
	codeStart := "\x0f`\x11\x02\x030,14"
	codeEnd := "\x11\x0f`"

	tests := []struct {
		name      string
		opts      ProcessMessageOpts
		input     string
		wantLines []string
	}{
		{
			name: "aligned table with left right center (unicode)",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "| Name | Score | Status |\n" +
				"| :--- | ---: | :---: |\n" +
				"| Alice | 100 | OK |\n" +
				"| Bob | 9 | Pass |",
			wantLines: []string{
				"┌───────┬───────┬────────┐",
				"│ \x02Name\x02  │ \x02Score\x02 │ \x02Status\x02 │",
				"├───────┼───────┼────────┤",
				"│ Alice │   100 │   OK   │",
				"│ Bob   │     9 │  Pass  │",
				"└───────┴───────┴────────┘",
			},
		},
		{
			name: "aligned table with left right center (ascii)",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: false},
			input: "| Name | Score | Status |\n" +
				"| :--- | ---: | :---: |\n" +
				"| Alice | 100 | OK |\n" +
				"| Bob | 9 | Pass |",
			wantLines: []string{
				"+-------+-------+--------+",
				"| \x02Name\x02  | \x02Score\x02 | \x02Status\x02 |",
				"+-------+-------+--------+",
				"| Alice |   100 |   OK   |",
				"| Bob   |     9 |  Pass  |",
				"+-------+-------+--------+",
			},
		},
		{
			name: "table without outer pipes",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "Col1 | Col2\n" +
				"--- | ---\n" +
				"a | b",
			wantLines: []string{
				"┌──────┬──────┐",
				"│ \x02Col1\x02 │ \x02Col2\x02 │",
				"├──────┼──────┤",
				"│ a    │ b    │",
				"└──────┴──────┘",
			},
		},
		{
			name: "table with inline markdown, inline code pipe, and escaped pipe",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "| **Col** | Code | Escaped |\n" +
				"| --- | --- | --- |\n" +
				"| *val* | `a | b` | x \\| y |\n" +
				"| `**no**` | foo \\\\| bar |",
			wantLines: []string{
				"┌──────────┬─────────┬─────────┐",
				"│ \x02Col\x02      │ \x02Code\x02    │ \x02Escaped\x02 │",
				"├──────────┼─────────┼─────────┤",
				"│ \x1dval\x1d      │ " + codeStart + "a | b" + codeEnd + " │ x | y   │",
				"│ " + codeStart + "**no**" + codeEnd + " │ foo \\   │ bar     │",
				"└──────────┴─────────┴─────────┘",
			},
		},
		{
			name: "header-only table without body rows",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "| A | B |\n" +
				"| - | - |",
			wantLines: []string{
				"┌───┬───┐",
				"│ \x02A\x02 │ \x02B\x02 │",
				"└───┴───┘",
			},
		},
		{
			name: "short body row padded to header column count",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "| A | B | C |\n" +
				"| - | - | - |\n" +
				"| 1 |",
			wantLines: []string{
				"┌───┬───┬───┐",
				"│ \x02A\x02 │ \x02B\x02 │ \x02C\x02 │",
				"├───┼───┼───┤",
				"│ 1 │   │   │",
				"└───┴───┴───┘",
			},
		},
		{
			name: "non-table pipe lines are emitted normally",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "echo foo | grep bar\n" +
				"just normal text",
			wantLines: []string{
				"echo foo | grep bar",
				"just normal text",
			},
		},
		{
			name: "table not confused with blockquote when BlockquoteChar is pipe",
			opts: ProcessMessageOpts{DisableEmoji: true, BlockquoteChar: "|"},
			input: "| K | V |\n" +
				"| - | - |\n" +
				"| a | b |",
			wantLines: []string{
				"+---+---+",
				"| \x02K\x02 | \x02V\x02 |",
				"+---+---+",
				"| a | b |",
				"+---+---+",
			},
		},
		{
			name: "quoted table preserves blockquote prefix",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true, BlockquoteChar: "\x0310🮇\x0f\u00a0"},
			input: "> | A | B |\n" +
				"> | - | - |\n" +
				"> | 1 | 2 |",
			wantLines: []string{
				"\x0310🮇\x0f\u00a0┌───┬───┐",
				"\x0310🮇\x0f\u00a0│ \x02A\x02 │ \x02B\x02 │",
				"\x0310🮇\x0f\u00a0├───┼───┤",
				"\x0310🮇\x0f\u00a0│ 1 │ 2 │",
				"\x0310🮇\x0f\u00a0└───┴───┘",
			},
		},
		{
			name: "table with trailing edited suffix",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "| A | B |\n" +
				"| - | - |\n" +
				"| 1 | 2 | \x1d(✏️edited)\x1d",
			wantLines: []string{
				"┌───┬───┐",
				"│ \x02A\x02 │ \x02B\x02 │",
				"├───┼───┤",
				"│ 1 │ 2 │",
				"└───┴───┘",
				"\x1d(✏️edited)\x1d",
			},
		},
		{
			name: "table with trailing reply suffix",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "| A | B |\n" +
				"| - | - |\n" +
				"| 1 | 2 | (re @user: hello)",
			wantLines: []string{
				"┌───┬───┐",
				"│ \x02A\x02 │ \x02B\x02 │",
				"├───┼───┤",
				"│ 1 │ 2 │",
				"└───┴───┘",
				"(re @user: hello)",
			},
		},
		{
			name: "pipe-less table cell starting with (re is not stripped as reply suffix",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "Topic | Note\n" +
				"--- | ---\n" +
				"Build | (re testing)",
			wantLines: []string{
				"┌───────┬──────────────┐",
				"│ \x02Topic\x02 │ \x02Note\x02         │",
				"├───────┼──────────────┤",
				"│ Build │ (re testing) │",
				"└───────┴──────────────┘",
			},
		},
		{
			name: "table surrounded by text and heading",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true, PreserveNewLines: "max-one"},
			input: "# Report\n\n" +
				"| Item | Qty |\n" +
				"| --- | --: |\n" +
				"| Apples | 12 |\n\n" +
				"---",
			wantLines: []string{
				"═══ \x02Report\x02 ═══",
				"",
				"┌────────┬─────┐",
				"│ \x02Item\x02   │ \x02Qty\x02 │",
				"├────────┼─────┤",
				"│ Apples │  12 │",
				"└────────┴─────┘",
				"",
				horizontalRuleLine,
			},
		},
		{
			name: "escaped backticks inside table cells do not swallow column separator",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: true},
			input: "| A | B |\n" +
				"| - | - |\n" +
				"| \\`a | b\\` |",
			wantLines: []string{
				"┌────┬────┐",
				"│ \x02A\x02  │ \x02B\x02  │",
				"├────┼────┤",
				"│ `a │ b` │",
				"└────┴────┘",
			},
		},
		{
			name: "table surrounded by text and heading (ascii)",
			opts: ProcessMessageOpts{DisableEmoji: true, Unicode: false, PreserveNewLines: "max-one"},
			input: "# Report\n\n" +
				"| Item | Qty |\n" +
				"| --- | --: |\n" +
				"| Apples | 12 |\n\n" +
				"---",
			wantLines: []string{
				"=== \x02Report\x02 ===",
				"",
				"+--------+-----+",
				"| \x02Item\x02   | \x02Qty\x02 |",
				"+--------+-----+",
				"| Apples |  12 |",
				"+--------+-----+",
				"",
				horizontalRuleLineASCII,
			},
		},
		{
			name: "disable markdown bypasses table parsing",
			opts: ProcessMessageOpts{DisableMarkdown: true, DisableEmoji: true},
			input: "| A | B |\n" +
				"| - | - |\n" +
				"| 1 | 2 |",
			wantLines: []string{
				"| A | B |",
				"| - | - |",
				"| 1 | 2 |",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collectProcessedLines(tc.input, tc.opts)
			if !slices.Equal(got, tc.wantLines) {
				t.Errorf("got:\n%q\nwant:\n%q", got, tc.wantLines)
			}
		})
	}
}

func TestFormatAndShortenSummaryMultiline(t *testing.T) {
	codeStart, codeEnd := "\x0f`\x11\x02\x030,14", "\x11\x0f`"

	tests := []struct {
		name  string
		opts  SummaryOpts
		input string
		want  string
	}{
		{
			name:  "heading on line 1 does not bleed into body (unicode)",
			opts:  SummaryOpts{DisableEmoji: true, Unicode: true},
			input: "# Title\nBody text",
			want:  "═══ \x02Title\x02 ═══ Body text",
		},
		{
			name:  "heading on line 1 does not bleed into body (ascii)",
			opts:  SummaryOpts{DisableEmoji: true, Unicode: false},
			input: "# Title\nBody text",
			want:  "=== \x02Title\x02 === Body text",
		},
		{
			name:  "multiline list formatted per line (unicode)",
			opts:  SummaryOpts{DisableEmoji: true, Unicode: true},
			input: "- first\n- second",
			want:  "• first • second",
		},
		{
			name:  "multiline list formatted per line (ascii)",
			opts:  SummaryOpts{DisableEmoji: true, Unicode: false},
			input: "- first\n- second",
			want:  "* first * second",
		},
		{
			name:  "fenced code block rendered as inline code",
			opts:  SummaryOpts{DisableEmoji: true, Unicode: true},
			input: "Check:\n```go\nfmt.Println(1)\n```",
			want:  "Check: " + codeStart + "go fmt.Println(1)" + codeEnd,
		},
		{
			name:  "four-backtick fenced code block containing triple backticks",
			opts:  SummaryOpts{DisableEmoji: true, Unicode: true},
			input: "Check:\n````md\n```\ncode\n```\n````",
			want:  "Check: " + codeStart + "md ``` code ```" + codeEnd,
		},
		{
			name:  "disable markdown preserves raw text with spaces",
			opts:  SummaryOpts{DisableMarkdown: true, DisableEmoji: true},
			input: "# Title\n**raw**",
			want:  "# Title **raw**",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatAndShortenSummary(tc.input, tc.opts); got != tc.want {
				t.Errorf("FormatAndShortenSummary(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
