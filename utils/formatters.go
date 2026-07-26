package utils

import (
	"bytes"
	"strconv"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/kenshaw/emoji"
)

//nolint:funlen,gocyclo
func FormatCodeBlockText(text string, codeBlockBackTick bool, codeBlockTilde bool, lexer string, syntaxHighlighting string, linePrefix string) (string, bool, bool, string) {
	if linePrefix != "" && strings.ContainsRune(linePrefix, '\\') {
		if unq, err := strconv.Unquote(`"` + linePrefix + `"`); err == nil {
			linePrefix = unq
		}
	}

	trimmedText := strings.TrimLeft(text, " \t")

	// Inline backtick toggle logic to avoid closure allocations
	if strings.HasPrefix(trimmedText, "```") && !codeBlockTilde {
		codeBlockBackTick = !codeBlockBackTick
		if codeBlockBackTick {
			newLexer := strings.TrimSpace(strings.TrimPrefix(trimmedText, "```"))
			if newLexer != "" {
				lexer = newLexer
				return linePrefix + "\x16" + lexer + "\x16", codeBlockBackTick, codeBlockTilde, lexer
			}
		} else {
			lexer = ""
		}
		return "", codeBlockBackTick, codeBlockTilde, lexer
	}

	// Inline tilde toggle logic
	if strings.HasPrefix(trimmedText, "~~~") && !codeBlockBackTick {
		codeBlockTilde = !codeBlockTilde
		if codeBlockTilde {
			newLexer := strings.TrimSpace(strings.TrimPrefix(trimmedText, "~~~"))
			if newLexer != "" {
				lexer = newLexer
				return linePrefix + "\x16" + lexer + "\x16", codeBlockBackTick, codeBlockTilde, lexer
			}
		} else {
			lexer = ""
		}
		return "", codeBlockBackTick, codeBlockTilde, lexer
	}

	codeBlock := codeBlockBackTick || codeBlockTilde
	if !codeBlock {
		return text, codeBlockBackTick, codeBlockTilde, lexer
	}

	if text == "" {
		return linePrefix + " ", codeBlockBackTick, codeBlockTilde, lexer
	}

	if syntaxHighlighting == "" || lexer == "" {
		// Use native string concatenation instead of a Builder for simple 2-string joins
		return linePrefix + text, codeBlockBackTick, codeBlockTilde, lexer
	}

	formatter := "terminal256"
	style := "pygments"
	if idx := strings.IndexByte(syntaxHighlighting, ':'); idx >= 0 {
		formatter = syntaxHighlighting[:idx]
		style = syntaxHighlighting[idx+1:]
	}

	// Single buffer approach: pre-allocate enough capacity to prevent reallocation
	var b bytes.Buffer
	b.Grow(len(linePrefix) + len(text) + 64)
	b.WriteString(linePrefix)

	if err := quick.Highlight(&b, text, lexer, formatter, style); err == nil {
		bs := b.Bytes()
		const resetSeq = "\x1b[0m"
		hasReset := bytes.HasSuffix(bs, []byte(resetSeq))

		end := len(bs)
		if hasReset {
			end -= len(resetSeq)
		}

		// Work around https://github.com/alecthomas/chroma/issues/716
		// Safely strip the trailing newline without touching the linePrefix
		if end > len(linePrefix) && bs[end-1] == '\n' {
			end--
		}

		// If we need to modify the tail, do it in-place using the buffer
		if end != len(bs) {
			b.Truncate(end)
			if hasReset {
				b.WriteString(resetSeq)
			}
		}

		return b.String(), codeBlockBackTick, codeBlockTilde, lexer
	}

	// Fallback if highlight fails
	return linePrefix + text, codeBlockBackTick, codeBlockTilde, lexer
}

// isWordChar mimics ASCII \w (letters, digits, and underscores)
func isWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}

func checkWordBoundaryStart(s string, idx int) bool {
	if idx == 0 {
		return true
	}
	return !isWordChar(s[idx-1]) // Must transition from a non-word char
}

func checkWordBoundaryEnd(s string, idx int) bool {
	if idx == len(s) {
		return true
	}
	return !isWordChar(s[idx]) // Must transition to a non-word char
}

// replacePattern simulates greedy regex matching like \*\*([^\*]+)\*\*
//
//nolint:funlen,gocyclo
func replacePattern(s string, delim string, ircStart, ircEnd string, checkWordBoundary bool) string {
	delimLen := len(delim)
	delimByte := delim[0] // '*' or '_'

	var b strings.Builder
	start := 0
	i := 0

	for i < len(s) {
		idx := strings.Index(s[i:], delim)
		if idx == -1 {
			break
		}
		absoluteIdx := i + idx

		// Ensure it's exactly the length we want (e.g., "**" shouldn't match "***")
		if absoluteIdx > 0 && s[absoluteIdx-1] == delimByte {
			i = absoluteIdx + 1
			continue
		}
		if absoluteIdx+delimLen < len(s) && s[absoluteIdx+delimLen] == delimByte {
			i = absoluteIdx + 1
			continue
		}

		if checkWordBoundary && !checkWordBoundaryStart(s, absoluteIdx) {
			i = absoluteIdx + 1
			continue
		}

		// Find the closing delimiter
		searchStart := absoluteIdx + delimLen
		closeIdx := strings.Index(s[searchStart:], delim)
		if closeIdx == -1 {
			i = absoluteIdx + 1
			continue
		}
		absoluteCloseIdx := searchStart + closeIdx

		// Ensure closing delimiter is exact length
		if absoluteCloseIdx+delimLen < len(s) && s[absoluteCloseIdx+delimLen] == delimByte {
			i = absoluteCloseIdx + 1
			continue
		}

		// Ensure inner string doesn't contain the delimiter character
		inner := s[searchStart:absoluteCloseIdx]
		if strings.IndexByte(inner, delimByte) != -1 {
			i = absoluteIdx + 1
			continue
		}

		if checkWordBoundary && !checkWordBoundaryEnd(s, absoluteCloseIdx+delimLen) {
			i = absoluteIdx + 1
			continue
		}

		// Lazy allocation only if we successfully found a valid pair
		if start == 0 {
			b.Grow(len(s) + 32)
		}

		b.WriteString(s[start:absoluteIdx])
		b.WriteString(ircStart)
		b.WriteString(inner)
		b.WriteString(ircEnd)

		start = absoluteCloseIdx + delimLen
		i = start
	}

	if start == 0 {
		return s // Zero allocations if no matches were made
	}
	b.WriteString(s[start:])
	return b.String()
}

// replaceCode simulates the `+([^`]+)`+ regex for backticks
func replaceCode(msg, startCode, endCode string) string {
	var b strings.Builder
	start := 0
	i := 0

	for i < len(msg) {
		idx := strings.IndexByte(msg[i:], '`')
		if idx == -1 {
			break
		}
		absoluteIdx := i + idx

		runLen := 1
		for absoluteIdx+runLen < len(msg) && msg[absoluteIdx+runLen] == '`' {
			runLen++
		}

		searchStart := absoluteIdx + runLen
		closeIdx := strings.IndexByte(msg[searchStart:], '`')
		if closeIdx == -1 {
			break
		}
		absoluteCloseIdx := searchStart + closeIdx

		closeRunLen := 1
		for absoluteCloseIdx+closeRunLen < len(msg) && msg[absoluteCloseIdx+closeRunLen] == '`' {
			closeRunLen++
		}

		if start == 0 {
			b.Grow(len(msg) + 32)
		}
		b.WriteString(msg[start:absoluteIdx])
		b.WriteString(startCode)
		b.WriteString(msg[absoluteIdx+runLen : absoluteCloseIdx])
		b.WriteString(endCode)

		start = absoluteCloseIdx + closeRunLen
		i = start
	}

	if start == 0 {
		return msg
	}
	b.WriteString(msg[start:])
	return b.String()
}

const blockQuoteCharDefault = ">"

func Markdown2irc(msg string, blockQuoteChar string, inlineCode string) string {
	if !strings.ContainsAny(msg, "*_`>") {
		return msg
	}

	// Block quotes
	trimmedText := strings.TrimLeft(msg, " \t")
	if strings.HasPrefix(trimmedText, blockQuoteCharDefault) && blockQuoteChar != blockQuoteCharDefault {
		if unq, err := strconv.Unquote(`"` + blockQuoteChar + `"`); err == nil {
			blockQuoteChar = unq
		}
		msg = strings.Replace(msg, blockQuoteCharDefault, blockQuoteChar, 1)
	}

	// Bold & Italic 0x02+0x1d - Asterisk processing
	if strings.Contains(msg, "*") {
		msg = replacePattern(msg, "***", "\x02\x1d", "\x1d\x02", false)
		msg = replacePattern(msg, "**", "\x02", "\x02", false)
		msg = replacePattern(msg, "*", "\x1d", "\x1d", false)
	}

	// Bold & Italic 0x02+0x1d - Underscore processing
	if strings.Contains(msg, "_") {
		msg = replacePattern(msg, "___", "\x02\x1d", "\x1d\x02", true)
		msg = replacePattern(msg, "__", "\x02", "\x02", true)
		msg = replacePattern(msg, "_", "\x1d", "\x1d", true)
	}

	// Code / Monospace 0x11
	if strings.Contains(msg, "`") {
		inlineCodeStart := "\x0f`\x11\x02\x030,14"
		inlineCodeEnd := "\x11\x0f`"

		if inlineCode != "" {
			if unq, err := strconv.Unquote(`"` + inlineCode + `"`); err == nil {
				inlineCode = unq
			}
			inlineCodeStart = inlineCode
			// Remove fence if not present
			if !strings.Contains(inlineCode, "`") {
				inlineCodeEnd = "\x11\x0f"
			}
		}
		// Not all IRC clients support monospace (0x11) so keep the fence
		msg = replaceCode(msg, inlineCodeStart, inlineCodeEnd)
	}

	return msg
}

var (
	emojiInitOnce sync.Once
	emojiData     []emoji.Emoji
	emojiAliasMap map[string]int
)

type emojiSkinToneInfo struct {
	suffix string
	match  string
	tone   emoji.SkinTone
}

var emojiSkinTones = []emojiSkinToneInfo{
	{"light", "_light", emoji.Light},
	{"medium_light", "_medium_light", emoji.MediumLight},
	{"medium", "_medium", emoji.Medium},
	{"medium_dark", "_medium_dark", emoji.MediumDark},
	{"dark", "_dark", emoji.Dark},
}

func initEmoji() {
	data := emoji.Gemoji()

	emojiAliasMap = make(map[string]int, len(data))

	for i, e := range data {
		if e.Emoji == "" {
			continue
		}
		for _, alias := range e.Aliases {
			if alias == "" {
				continue
			}
			emojiAliasMap[alias] = i
		}
		// In addition to emoji aliases, include emoji tags
		for _, tag := range e.Tags {
			if tag == "" {
				continue
			}
			// But only if it doesn't already exist, e.g. "angry"
			if _, ok := emojiAliasMap[tag]; !ok {
				emojiAliasMap[tag] = i
			}
		}
	}

	emojiData = data
}

func EmojiReplaceAliases(s string) string {
	if strings.IndexByte(s, ':') < 0 {
		return s
	}

	emojiInitOnce.Do(initEmoji)

	var builder strings.Builder
	builder.Grow(len(s))

	start := -1
	for i := range len(s) {
		// Handle normal characters outside of colons
		if s[i] != ':' {
			if start == -1 {
				builder.WriteByte(s[i])
			}
			continue
		}

		// We found a colon, mark it as the start of a potential emoji
		if start == -1 {
			start = i
			continue
		}

		// We found a second colon, test the substring
		code := s[start : i+1]
		if emojiStr, ok := EmojiFromAlias(code); ok {
			builder.WriteString(emojiStr)
			start = -1 // Reset for the next emoji
		} else {
			// Not a valid emoji. Write everything up to this colon,
			// and treat this current colon as the new start.
			builder.WriteString(s[start:i])
			start = i
		}
	}

	// Write any unclosed trailing colons/text
	if start != -1 {
		builder.WriteString(s[start:])
	}

	return builder.String()
}

func EmojiFromAlias(alias string) (string, bool) {
	if alias == "" {
		return "", false
	}

	emojiInitOnce.Do(initEmoji)

	if a, ok := strings.CutPrefix(alias, ":"); ok {
		if a, ok := strings.CutSuffix(a, ":"); ok {
			alias = a
		}
	}

	if idx, ok := emojiAliasMap[alias]; ok {
		return emojiData[idx].Emoji, true
	}

	base, ok := strings.CutSuffix(alias, "_skin_tone")
	if !ok {
		return "", false
	}

	// Support skin tones
	for _, st := range emojiSkinTones {
		if !strings.HasSuffix(base, st.match) {
			continue
		}

		baseAlias := strings.TrimSuffix(base, st.match)

		idx, ok := emojiAliasMap[baseAlias]
		if !ok {
			return "", false
		}

		return emojiData[idx].Tone(st.tone), true
	}

	return "", false
}

// WrapMessage soft-wraps msg into lines of at most maxLen bytes, breaking
// only at spaces/newlines. Words longer than maxLen are left unbroken and
// overflow their line rather than being split - fine here since IRC's real
// line limit (512) leaves headroom above maxLen (440), and splitting mid-
// word would corrupt URLs/tokens. Pure byte scanning throughout
//
//nolint:gocyclo
func WrapMessage(msg string, maxLen int) string {
	if maxLen <= 0 || len(msg) <= maxLen {
		return msg
	}

	var b strings.Builder
	b.Grow(len(msg) + (len(msg)/maxLen)*2)

	lineLen := 0
	spaceStart, spaceLen := 0, 0

	i := 0
	for i < len(msg) {
		switch {
		case msg[i] == ' ' || msg[i] == '\t':
			spaceStart = i
			for i < len(msg) && (msg[i] == ' ' || msg[i] == '\t') {
				i++
			}
			spaceLen = i - spaceStart

		case msg[i] == '\n':
			b.WriteByte('\n')
			lineLen, spaceLen = 0, 0
			i++

		default:
			start := i
			for i < len(msg) && msg[i] != ' ' && msg[i] != '\t' && msg[i] != '\n' {
				i++
			}
			word := msg[start:i]

			if lineLen > 0 && lineLen+spaceLen+len(word) > maxLen {
				b.WriteByte('\n')
				lineLen, spaceLen = 0, 0 // drop pending spaces, don't carry to new line
			} else if spaceLen > 0 {
				b.WriteString(msg[spaceStart : spaceStart+spaceLen])
				lineLen += spaceLen
				spaceLen = 0
			}

			b.WriteString(word)
			lineLen += len(word)
		}
	}

	return b.String()
}
