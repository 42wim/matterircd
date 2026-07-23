package utils

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/kenshaw/emoji"
)

//nolint:funlen,gocyclo
func FormatCodeBlockText(text string, codeBlockBackTick bool, codeBlockTilde bool, lexer string, syntaxHighlighting string, linePrefix string) (string, bool, bool, string) {
	if linePrefix != "" {
		if unq, err := strconv.Unquote(`"` + linePrefix + `"`); err == nil {
			linePrefix = unq
		}
	}

	trimmedText := strings.TrimLeft(text, " \t")

	handleToggle := func(prefix string, isActive bool) string {
		if isActive {
			newLexer := strings.TrimSpace(strings.TrimPrefix(trimmedText, prefix))
			if newLexer != "" {
				lexer = newLexer
				return linePrefix + "\x16" + lexer + "\x16"
			}
			return ""
		}
		lexer = ""
		return ""
	}

	if strings.HasPrefix(trimmedText, "```") && !codeBlockTilde {
		codeBlockBackTick = !codeBlockBackTick
		return handleToggle("```", codeBlockBackTick), codeBlockBackTick, codeBlockTilde, lexer
	}

	if strings.HasPrefix(trimmedText, "~~~") && !codeBlockBackTick {
		codeBlockTilde = !codeBlockTilde
		return handleToggle("~~~", codeBlockTilde), codeBlockBackTick, codeBlockTilde, lexer
	}

	codeBlock := codeBlockBackTick || codeBlockTilde
	if !codeBlock {
		return text, codeBlockBackTick, codeBlockTilde, lexer
	}

	var sb strings.Builder
	sb.WriteString(linePrefix)

	if text == "" {
		sb.WriteByte(' ')
		return sb.String(), codeBlockBackTick, codeBlockTilde, lexer
	}

	if syntaxHighlighting == "" || lexer == "" {
		sb.WriteString(text)
		return sb.String(), codeBlockBackTick, codeBlockTilde, lexer
	}

	formatter := "terminal256"
	style := "pygments"
	if idx := strings.IndexByte(syntaxHighlighting, ':'); idx >= 0 {
		formatter = syntaxHighlighting[:idx]
		style = syntaxHighlighting[idx+1:]
	}

	var b bytes.Buffer
	if err := quick.Highlight(&b, text, lexer, formatter, style); err == nil {
		bs := b.Bytes()
		// Work around https://github.com/alecthomas/chroma/issues/716
		const resetSeq = "\x1b[0m"
		hasReset := bytes.HasSuffix(bs, []byte(resetSeq))
		if hasReset {
			bs = bs[:len(bs)-len(resetSeq)]
		}
		if len(bs) > 0 && bs[len(bs)-1] == '\n' {
			bs = bs[:len(bs)-1]
		}
		if hasReset {
			bs = append(bs, resetSeq...)
		}

		sb.Write(bs)
	} else {
		sb.WriteString(text)
	}

	return sb.String(), codeBlockBackTick, codeBlockTilde, lexer
}

// Use static initialisation to optimize.
// Bold & Italic - https://www.markdownguide.org/basic-syntax#bold-and-italic
var boldItalicRegExp = []*regexp.Regexp{
	regexp.MustCompile(`\*\*\*([^\*]+)\*\*\*`),
	regexp.MustCompile(`\b\_\_\_([^\_]+)\_\_\_\b`),
}

// Bold - https://www.markdownguide.org/basic-syntax#bold
var boldRegExp = []*regexp.Regexp{
	regexp.MustCompile(`\*\*([^\*]+)\*\*`),
	regexp.MustCompile(`\b\_\_([^\_]+)\_\_\b`),
}

// Italic - https://www.markdownguide.org/basic-syntax#italic
var italicRegExp = []*regexp.Regexp{
	regexp.MustCompile(`\*([^\*]+)\*`),
	regexp.MustCompile(`\b\_([^\_]+)\_\b`),
}

// Code / Monospace - https://markdownguide.offshoot.io/basic-syntax/#code
var codeRegExp = []*regexp.Regexp{
	regexp.MustCompile("`+([^`]+)`+"),
}

const blockQuoteCharDefault = ">"

func Markdown2irc(msg string, blockQuoteChar string, inlineCode string) string {
	if !strings.ContainsAny(msg, "*_`>") {
		return msg
	}

	// Bold & Italic 0x02+0x1d
	if strings.ContainsAny(msg, "*_") {
		for _, re := range boldItalicRegExp {
			msg = re.ReplaceAllString(msg, "\x02\x1d${1}\x1d\x02")
		}

		// Bold 0x02
		for _, re := range boldRegExp {
			msg = re.ReplaceAllString(msg, "\x02${1}\x02")
		}

		// Italic 0x1d
		for _, re := range italicRegExp {
			msg = re.ReplaceAllString(msg, "\x1d${1}\x1d")
		}
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
		for _, re := range codeRegExp {
			// Not all IRC clients support monospace (0x11) so keep the fence
			msg = re.ReplaceAllString(msg, inlineCodeStart+"${1}"+inlineCodeEnd)
		}
	}

	// Block quotes
	trimmedText := strings.TrimLeft(msg, " \t")
	if strings.HasPrefix(trimmedText, blockQuoteCharDefault) && blockQuoteChar != blockQuoteCharDefault {
		if unq, err := strconv.Unquote(`"` + blockQuoteChar + `"`); err == nil {
			blockQuoteChar = unq
		}
		msg = strings.Replace(msg, blockQuoteCharDefault, blockQuoteChar, 1)
	}

	return msg
}

var (
	emojiInitOnce sync.Once
	emojiData     []emoji.Emoji
	emojiReplacer *strings.Replacer
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

	var aliasPairs []string
	emojiAliasMap = make(map[string]int, len(data))

	for i, e := range data {
		if e.Emoji == "" {
			continue
		}
		for _, alias := range e.Aliases {
			if alias == "" {
				continue
			}
			emojiAliasMap[alias], aliasPairs = i, append(aliasPairs, ":"+alias+":", e.Emoji)
			if !e.SkinTones {
				continue
			}
			// Include skin tones
			for _, st := range emojiSkinTones {
				aliasTone := alias + "_" + st.suffix + "_skin_tone"
				aliasPairs = append(aliasPairs, ":"+aliasTone+":", e.Tone(st.tone))
			}
		}
		// In addition to emoji aliases, include emoji tags
		for _, tag := range e.Tags {
			if tag == "" {
				continue
			}
			// But only if it doesn't already exist, e.g. "angry"
			if _, ok := emojiAliasMap[tag]; !ok {
				emojiAliasMap[tag], aliasPairs = i, append(aliasPairs, ":"+tag+":", e.Emoji)
				if !e.SkinTones {
					continue
				}
				// Include skin tones
				for _, st := range emojiSkinTones {
					aliasTone := tag + "_" + st.suffix + "_skin_tone"
					aliasPairs = append(aliasPairs, ":"+aliasTone+":", e.Tone(st.tone))
				}
			}
		}
	}

	emojiData = data
	emojiReplacer = strings.NewReplacer(aliasPairs...)
}

func EmojiReplaceAliases(s string) string {
	if strings.IndexByte(s, ':') < 0 {
		return s
	}
	emojiInitOnce.Do(initEmoji)
	return emojiReplacer.Replace(s)
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
func WrapMessage(msg string, maxLen int) string {
	if maxLen <= 0 || len(msg) <= maxLen {
		return msg
	}

	var b strings.Builder
	b.Grow(len(msg) + (len(msg) / maxLen) * 2)

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
