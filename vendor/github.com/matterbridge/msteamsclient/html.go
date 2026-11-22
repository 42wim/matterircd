package msteamsclient

import (
	"fmt"
	stdhtml "html"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

const (
	skypeReplyItemType = "http://schema.skype.com/Reply"
	skypeEmojiItemType = "http://schema.skype.com/Emoji"
)

var (
	mdCodeBlockRE = regexp.MustCompile("(?s)```(?:([a-zA-Z0-9_+-]+)\\n)?(.*?)```")
	mdInlineCode  = regexp.MustCompile("`([^`\n]+)`")
	mdBoldRE      = regexp.MustCompile(`\*\*([^\*\n]+)\*\*`)
	mdStrikeRE    = regexp.MustCompile(`~~([^~\n]+)~~`)
	mdItalicRE    = regexp.MustCompile(`(?:^|[^\*])\*([^\*\n]+)\*(?:[^\*]|$)`)
)

// HTMLToText converts Microsoft Teams RichText/Html message content into clean
// Markdown/IRC-friendly text, omitting embedded Skype reply quote blocks
// (whose target ID can be obtained via ExtractReplyParentID).
func HTMLToText(rawHTML string) string {
	if rawHTML == "" {
		return ""
	}

	if !strings.ContainsRune(rawHTML, '<') {
		return strings.TrimSpace(stdhtml.UnescapeString(rawHTML))
	}

	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return strings.TrimSpace(stdhtml.UnescapeString(rawHTML))
	}

	var b strings.Builder

	renderHTMLNode(&b, doc, false)

	return normalizeRenderedText(b.String())
}

// ExtractReplyParentID returns the quoted parent message ID from a Teams
// chat reply blockquote, or "" if the HTML does not contain one.
func ExtractReplyParentID(rawHTML string) string {
	if !strings.Contains(rawHTML, "Reply") {
		return ""
	}

	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return ""
	}

	return findReplyItemID(doc)
}

func findReplyItemID(n *html.Node) string {
	if n.Type == html.ElementNode && n.Data == "blockquote" {
		if getAttr(n, "itemtype") == skypeReplyItemType {
			if id := strings.TrimSpace(getAttr(n, "itemid")); id != "" {
				return id
			}
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if id := findReplyItemID(c); id != "" {
			return id
		}
	}

	return ""
}

func renderHTMLNode(b *strings.Builder, n *html.Node, inPre bool) {
	switch n.Type {
	case html.TextNode:
		if inPre {
			b.WriteString(n.Data)

			return
		}

		b.WriteString(collapseInlineWhitespace(n.Data))

		return
	case html.ElementNode:
		renderElementNode(b, n, inPre)

		return
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderHTMLNode(b, c, inPre)
		}
	}
}

//nolint:funlen,gocyclo
func renderElementNode(b *strings.Builder, n *html.Node, inPre bool) {
	switch n.Data {
	case "script", "style", "head":
		return
	case "br":
		b.WriteByte('\n')

		return
	case "p", "div":
		ensureNewline(b)
		renderChildren(b, n, inPre)
		ensureNewline(b)

		return
	case "li":
		ensureNewline(b)
		b.WriteString("- ")
		renderChildren(b, n, inPre)
		ensureNewline(b)

		return
	case "b", "strong":
		renderWrapped(b, n, "**", "**", inPre)

		return
	case "i", "em":
		renderWrapped(b, n, "*", "*", inPre)

		return
	case "s", "del", "strike":
		renderWrapped(b, n, "~~", "~~", inPre)

		return
	case "u":
		renderWrapped(b, n, "\x1f", "\x1f", inPre)

		return
	case "code":
		if inPre {
			renderChildren(b, n, true)

			return
		}

		renderWrapped(b, n, "`", "`", true)

		return
	case "pre":
		ensureNewline(b)
		b.WriteString("```")
		b.WriteString(extractCodeLanguage(n))
		b.WriteByte('\n')
		renderChildren(b, n, true)
		ensureNewline(b)
		b.WriteString("```\n")

		return
	case "blockquote":
		if getAttr(n, "itemtype") == skypeReplyItemType {
			return
		}

		var inner strings.Builder

		renderChildren(&inner, n, false)

		quoted := strings.TrimSpace(inner.String())
		if quoted == "" {
			return
		}

		ensureNewline(b)

		for line := range strings.SplitSeq(quoted, "\n") {
			b.WriteString("> ")
			b.WriteString(line)
			b.WriteByte('\n')
		}

		return
	case "a":
		renderAnchor(b, n, inPre)

		return
	case "at":
		var inner strings.Builder

		renderChildren(&inner, n, false)

		label := strings.TrimSpace(inner.String())
		if label != "" {
			if !strings.HasPrefix(label, "@") {
				b.WriteByte('@')
			}

			b.WriteString(label)
		}

		return
	case "emoji", "img":
		if alt := getAttr(n, "alt"); alt != "" {
			b.WriteString(alt)

			return
		}

		if getAttr(n, "itemtype") == skypeEmojiItemType {
			if title := getAttr(n, "title"); title != "" {
				b.WriteString(title)
			}
		}

		return
	default:
		renderChildren(b, n, inPre)
	}
}

func renderChildren(b *strings.Builder, n *html.Node, inPre bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderHTMLNode(b, c, inPre)
	}
}

func renderWrapped(b *strings.Builder, n *html.Node, openTag, closeTag string, inPre bool) {
	var inner strings.Builder

	renderChildren(&inner, n, inPre)

	content := inner.String()

	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		b.WriteString(content)

		return
	}

	lead := content[:len(content)-len(strings.TrimLeft(content, " \t\u00a0"))]
	trail := content[len(strings.TrimRight(content, " \t\u00a0")):]

	b.WriteString(lead)
	b.WriteString(openTag)
	b.WriteString(trimmed)
	b.WriteString(closeTag)
	b.WriteString(trail)
}

func renderAnchor(b *strings.Builder, n *html.Node, inPre bool) {
	href := strings.TrimSpace(getAttr(n, "href"))

	var inner strings.Builder

	renderChildren(&inner, n, inPre)

	label := strings.TrimSpace(inner.String())

	switch {
	case href == "":
		b.WriteString(label)
	case label == "" || label == href:
		b.WriteString(href)
	default:
		b.WriteString(label)
		b.WriteString(" (")
		b.WriteString(href)
		b.WriteByte(')')
	}
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}

	return ""
}

func extractCodeLanguage(preNode *html.Node) string {
	candidates := []*html.Node{preNode}
	for c := preNode.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "code" {
			candidates = append(candidates, c)
		}
	}

	if preNode.Parent != nil && preNode.Parent.Type == html.ElementNode {
		candidates = append(candidates, preNode.Parent)
	}

	for _, node := range candidates {
		if lang := codeLanguageFromNode(node); lang != "" {
			return lang
		}
	}

	return ""
}

func codeLanguageFromNode(n *html.Node) string {
	for _, attr := range []string{"data-language", "data-lang", "language", "lang"} {
		if lang := normalizeCodeLanguage(getAttr(n, attr)); lang != "" {
			return lang
		}
	}

	for cls := range strings.FieldsSeq(getAttr(n, "class")) {
		lower := strings.ToLower(cls)
		if after, ok := strings.CutPrefix(lower, "language-"); ok {
			if lang := normalizeCodeLanguage(after); lang != "" {
				return lang
			}
		}

		if after, ok := strings.CutPrefix(lower, "lang-"); ok {
			if lang := normalizeCodeLanguage(after); lang != "" {
				return lang
			}
		}
	}

	return ""
}

func normalizeCodeLanguage(raw string) string {
	lang := strings.ToLower(strings.TrimSpace(raw))
	switch lang {
	case "", "plaintext", "plain", "text", "txt", "none", "auto":
		return ""
	}

	for i := range len(lang) {
		ch := lang[i]

		isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
		if !isAlphaNum && ch != '_' && ch != '+' && ch != '-' && ch != '#' {
			return ""
		}
	}

	return lang
}

func ensureNewline(b *strings.Builder) {
	if b.Len() == 0 {
		return
	}

	s := b.String()
	if s[len(s)-1] != '\n' {
		b.WriteByte('\n')
	}
}

func collapseInlineWhitespace(s string) string {
	if s == "" {
		return ""
	}

	s = strings.ReplaceAll(s, "\u00a0", " ")

	var b strings.Builder

	b.Grow(len(s))

	lastSpace := false

	for i := range len(s) {
		ch := s[i]
		if ch == '\r' || ch == '\n' || ch == '\t' || ch == ' ' {
			if !lastSpace {
				b.WriteByte(' ')

				lastSpace = true
			}

			continue
		}

		b.WriteByte(ch)

		lastSpace = false
	}

	return b.String()
}

func normalizeRenderedText(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blankRun := 0

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "" {
			blankRun++
			if blankRun <= 1 && len(out) > 0 {
				out = append(out, "")
			}

			continue
		}

		blankRun = 0

		out = append(out, trimmed)
	}

	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}

	return strings.Join(out, "\n")
}

func renderCodeBlockHTML(match string) string {
	sub := mdCodeBlockRE.FindStringSubmatch(match)

	lang := ""
	code := ""

	if len(sub) > 2 {
		lang = normalizeCodeLanguage(sub[1])
		code = sub[2]
	}

	openCode := "<code>"
	if lang != "" {
		openCode = `<code class="language-` + stdhtml.EscapeString(lang) + `">`
	}

	return "<pre>" + openCode + stdhtml.EscapeString(strings.TrimSuffix(code, "\n")) + "</code></pre>"
}

// TextToHTML converts plain or Markdown-formatted text into safe Teams
// RichText/Html.
func TextToHTML(text string) string {
	if text == "" {
		return ""
	}

	// Preserve fenced code blocks before escaping and inline formatting.
	var placeholders []string

	replaced := mdCodeBlockRE.ReplaceAllStringFunc(text, func(match string) string {
		idx := len(placeholders)
		placeholders = append(placeholders, renderCodeBlockHTML(match))

		return fmt.Sprintf("\x00CODEBLOCK%d\x00", idx)
	})

	escaped := stdhtml.EscapeString(replaced)

	escaped = mdInlineCode.ReplaceAllString(escaped, "<code>$1</code>")
	escaped = mdBoldRE.ReplaceAllString(escaped, "<b>$1</b>")
	escaped = mdStrikeRE.ReplaceAllString(escaped, "<s>$1</s>")
	escaped = mdItalicRE.ReplaceAllStringFunc(escaped, func(match string) string {
		prefix := ""
		suffix := ""
		inner := match

		if !strings.HasPrefix(inner, "*") {
			prefix = inner[:1]
			inner = inner[1:]
		}

		if !strings.HasSuffix(inner, "*") {
			suffix = inner[len(inner)-1:]
			inner = inner[:len(inner)-1]
		}

		inner = strings.TrimPrefix(inner, "*")
		inner = strings.TrimSuffix(inner, "*")

		return prefix + "<i>" + inner + "</i>" + suffix
	})

	escaped = strings.ReplaceAll(escaped, "\r\n", "\n")
	escaped = strings.ReplaceAll(escaped, "\n", "<br/>")

	for i, block := range placeholders {
		escaped = strings.ReplaceAll(escaped, fmt.Sprintf("\x00CODEBLOCK%d\x00", i), block)
	}

	return "<p>" + escaped + "</p>"
}

// WrapReplyQuoteHTML prepends a Teams chat reply blockquote referencing parentID.
func WrapReplyQuoteHTML(parentMRI, parentAuthor, parentID, parentSnippet, bodyHTML string) string {
	var b strings.Builder

	b.WriteString(`<blockquote itemscope="" itemtype="`)
	b.WriteString(skypeReplyItemType)
	b.WriteString(`" itemid="`)
	b.WriteString(stdhtml.EscapeString(parentID))
	b.WriteString(`">`)

	if parentAuthor != "" || parentMRI != "" {
		b.WriteString(`<strong itemprop="mri" itemid="`)
		b.WriteString(stdhtml.EscapeString(parentMRI))
		b.WriteString(`">`)
		b.WriteString(stdhtml.EscapeString(parentAuthor))
		b.WriteString(`</strong>`)
	}

	if parentSnippet != "" {
		b.WriteString(`<p itemprop="preview">`)
		b.WriteString(stdhtml.EscapeString(parentSnippet))
		b.WriteString(`</p>`)
	}

	b.WriteString(`</blockquote>`)
	b.WriteString(bodyHTML)

	return b.String()
}
