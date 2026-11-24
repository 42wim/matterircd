package matrixclient

import (
	stdhtml "html"
	"strings"

	"maunium.net/go/mautrix/event"
)

func newTextMessageContent(text string) event.MessageEventContent {
	content := event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	}

	if formatted, ok := markdownToHTML(text); ok {
		content.Format = event.FormatHTML
		content.FormattedBody = formatted
	}

	return content
}

// markdownToHTML converts common Markdown (fenced code blocks, blockquotes,
// inline code, links, bold, italic, strikethrough) to Matrix HTML
// (org.matrix.custom.html). It returns ("", false) when text contains no
// Markdown formatting so plain messages stay plain m.text.
func markdownToHTML(text string) (string, bool) {
	if !strings.ContainsAny(text, "`*_~>[") {
		return "", false
	}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	var (
		b            strings.Builder
		hasFormatted bool
		needBR       bool
	)

	b.Grow(len(text) + 32)

	for i := 0; i < len(lines); i++ {
		if endIdx, blockHTML, ok := parseFencedCodeBlock(lines, i); ok {
			b.WriteString(blockHTML)

			hasFormatted = true
			needBR = false
			i = endIdx

			continue
		}

		if endIdx, quoteHTML, ok := parseBlockquoteLines(lines, i); ok {
			b.WriteString(quoteHTML)

			hasFormatted = true
			needBR = false
			i = endIdx

			continue
		}

		if needBR {
			b.WriteString("<br/>")
		}

		inlineHTML, formatted := renderInlineMarkdown(lines[i])
		b.WriteString(inlineHTML)

		if formatted {
			hasFormatted = true
		}

		needBR = true
	}

	if !hasFormatted {
		return "", false
	}

	return b.String(), true
}

func parseFencedCodeBlock(lines []string, start int) (int, string, bool) {
	trimmed := strings.TrimSpace(lines[start])
	if !strings.HasPrefix(trimmed, "```") {
		return start, "", false
	}

	lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
	if strings.ContainsAny(lang, " \t`") {
		return start, "", false
	}

	closeIdx := -1

	for j := start + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "```" {
			closeIdx = j

			break
		}
	}

	if closeIdx < 0 {
		return start, "", false
	}

	code := strings.Join(lines[start+1:closeIdx], "\n")
	if code != "" {
		code += "\n"
	}

	var b strings.Builder

	b.WriteString("<pre><code")

	if lang != "" {
		b.WriteString(` class="language-`)
		b.WriteString(stdhtml.EscapeString(lang))
		b.WriteByte('"')
	}

	b.WriteByte('>')
	b.WriteString(stdhtml.EscapeString(code))
	b.WriteString("</code></pre>")

	return closeIdx, b.String(), true
}

func parseBlockquoteLines(lines []string, start int) (int, string, bool) {
	if _, ok := cutBlockquoteLine(lines[start]); !ok {
		return start, "", false
	}

	var inner strings.Builder

	end := start

	for j := start; j < len(lines); j++ {
		content, ok := cutBlockquoteLine(lines[j])
		if !ok {
			break
		}

		if j > start {
			inner.WriteString("<br/>")
		}

		rendered, _ := renderInlineMarkdown(content)
		inner.WriteString(rendered)

		end = j
	}

	return end, "<blockquote><p>" + inner.String() + "</p></blockquote>", true
}

func cutBlockquoteLine(line string) (string, bool) {
	if after, ok := strings.CutPrefix(line, "> "); ok {
		return after, true
	}

	if line == ">" {
		return "", true
	}

	return "", false
}

func renderInlineMarkdown(s string) (string, bool) {
	var (
		b         strings.Builder
		formatted bool
	)

	b.Grow(len(s))

	for i := 0; i < len(s); {
		if rendered, next, ok := tryInlineMarkdown(s, i); ok {
			b.WriteString(rendered)

			formatted = true
			i = next

			continue
		}

		writeEscapedByte(&b, s[i])
		i++
	}

	return b.String(), formatted
}

func tryInlineMarkdown(s string, i int) (string, int, bool) {
	switch s[i] {
	case '`':
		return tryInlineCode(s, i)
	case '[':
		return tryInlineLink(s, i)
	case '~':
		return tryInlineDelim(s, i, "~~", "del")
	case '*':
		if rendered, next, ok := tryInlineDelim(s, i, "**", "strong"); ok {
			return rendered, next, true
		}

		return tryInlineDelim(s, i, "*", "em")
	case '_':
		return tryInlineUnderscore(s, i)
	default:
		return "", i, false
	}
}

func tryInlineCode(s string, i int) (string, int, bool) {
	end := strings.IndexByte(s[i+1:], '`')
	if end <= 0 {
		return "", i, false
	}

	code := s[i+1 : i+1+end]

	return "<code>" + stdhtml.EscapeString(code) + "</code>", i + 1 + end + 1, true
}

func tryInlineLink(s string, i int) (string, int, bool) {
	closeBracket := strings.IndexByte(s[i+1:], ']')
	if closeBracket <= 0 {
		return "", i, false
	}

	afterBracket := i + 1 + closeBracket + 1
	if afterBracket >= len(s) || s[afterBracket] != '(' {
		return "", i, false
	}

	closeParen := strings.IndexByte(s[afterBracket+1:], ')')
	if closeParen <= 0 {
		return "", i, false
	}

	label := s[i+1 : i+1+closeBracket]
	href := s[afterBracket+1 : afterBracket+1+closeParen]

	if strings.ContainsAny(href, " \t\r\n") || !isAllowedLinkScheme(href) {
		return "", i, false
	}

	renderedLabel, _ := renderInlineMarkdown(label)

	return `<a href="` + stdhtml.EscapeString(href) + `">` + renderedLabel + `</a>`, afterBracket + 1 + closeParen + 1, true
}

func isAllowedLinkScheme(href string) bool {
	lower := strings.ToLower(href)

	return strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "mailto:")
}

func tryInlineDelim(s string, i int, delim, tag string) (string, int, bool) {
	dlen := len(delim)
	if !strings.HasPrefix(s[i:], delim) {
		return "", i, false
	}

	start := i + dlen
	if start >= len(s) || s[start] == ' ' || s[start] == '\t' {
		return "", i, false
	}

	rel := strings.Index(s[start:], delim)
	if rel <= 0 {
		return "", i, false
	}

	end := start + rel
	if s[end-1] == ' ' || s[end-1] == '\t' {
		return "", i, false
	}

	inner, _ := renderInlineMarkdown(s[start:end])

	return "<" + tag + ">" + inner + "</" + tag + ">", end + dlen, true
}

func tryInlineUnderscore(s string, i int) (string, int, bool) {
	if i > 0 && isASCIIWordChar(s[i-1]) {
		return "", i, false
	}

	start := i + 1
	if start >= len(s) || s[start] == ' ' || s[start] == '\t' || s[start] == '_' {
		return "", i, false
	}

	for pos := start; pos < len(s); {
		rel := strings.IndexByte(s[pos:], '_')
		if rel < 0 {
			return "", i, false
		}

		end := pos + rel
		after := end + 1

		if end > start && s[end-1] != ' ' && s[end-1] != '\t' && (after >= len(s) || !isASCIIWordChar(s[after])) {
			inner, _ := renderInlineMarkdown(s[start:end])

			return "<em>" + inner + "</em>", after, true
		}

		pos = end + 1
	}

	return "", i, false
}

func isASCIIWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func writeEscapedByte(b *strings.Builder, c byte) {
	switch c {
	case '&':
		b.WriteString("&amp;")
	case '<':
		b.WriteString("&lt;")
	case '>':
		b.WriteString("&gt;")
	case '"':
		b.WriteString("&quot;")
	default:
		b.WriteByte(c)
	}
}
