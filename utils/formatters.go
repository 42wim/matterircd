package utils

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/kenshaw/emoji"
)

// IRCColor represents a pre-parsed IRC color for fast distance calculations.
type IRCColor struct {
	Code    string
	R, G, B int
}

var (
	// precalculatedPalette will hold our unique colors
	precalculatedPalette []IRCColor
	paletteOnce          sync.Once
)

func initializePalette() {
	// 00-98 raw hex codes
	rawColors := []string{
		// 00-15
		"FFFFFF", "000000", "00007F", "009300", "FF0000", "7F0000", "9C009C", "FC7F00",
		"FFFF00", "00FC00", "009393", "00FFFF", "0000FC", "FF00FF", "7F7F7F", "D2D2D2",
		// 16-31
		"470000", "472100", "474700", "324700", "004700", "00472C", "004747", "002747",
		"000047", "2E0047", "470047", "47002A", "740000", "743A00", "747400", "517400",
		// 32-47
		"007400", "007449", "007474", "004074", "000074", "4B0074", "740074", "740045",
		"B50000", "B56300", "B5B500", "7DB500", "00B500", "00B571", "00B5B5", "0063B5",
		// 48-63
		"0000B5", "7500B5", "B500B5", "B5006B", "FF0000", "FF8C00", "FFFF00", "B2FF00",
		"00FF00", "00FFA0", "00FFFF", "008CFF", "0000FF", "A500FF", "FF00FF", "FF0098",
		// 64-79
		"FF5959", "FFB459", "FFFF71", "CFFF60", "6FFF6F", "65FFC9", "6DFFFF", "59B4FF",
		"5959FF", "C459FF", "FF66FF", "FF59BC", "FF9C9C", "FFD39C", "FFFF9C", "E2FF9C",
		// 80-95
		"9CFF9C", "9CFFDB", "9CFFFF", "9CD3FF", "9C9CFF", "DC9CFF", "FF9CFF", "FF94D3",
		"000000", "131313", "282828", "363636", "4D4D4D", "656565", "818181", "9F9F9F",
		// 96-98
		"BCBCBC", "E2E2E2", "FFFFFF",
	}

	// Use a map to track and exclude duplicates.
	// Because we iterate from 0 to 98, standard codes (0-15) are saved first.
	seenHex := make(map[string]bool)

	for i, hex := range rawColors {
		if seenHex[hex] {
			continue
		}
		seenHex[hex] = true

		r, _ := strconv.ParseInt(hex[0:2], 16, 32)
		g, _ := strconv.ParseInt(hex[2:4], 16, 32)
		b, _ := strconv.ParseInt(hex[4:6], 16, 32)

		precalculatedPalette = append(precalculatedPalette, IRCColor{
			Code: fmt.Sprintf("%02d", i),
			R:    int(r), G: int(g), B: int(b),
		})
	}
}

// FindClosestIRCColor uses the precalculated palette to quickly find the nearest match.
func FindClosestIRCColor(hexColor string) string {
	paletteOnce.Do(initializePalette)

	hexColor = strings.ToUpper(strings.TrimPrefix(hexColor, "#"))
	if len(hexColor) != 6 {
		return "01"
	}

	r64, _ := strconv.ParseInt(hexColor[0:2], 16, 32)
	g64, _ := strconv.ParseInt(hexColor[2:4], 16, 32)
	b64, _ := strconv.ParseInt(hexColor[4:6], 16, 32)
	r1, g1, b1 := int(r64), int(g64), int(b64)

	minDist := math.MaxInt32
	bestCode := "01"

	for _, c := range precalculatedPalette {
		// Calculate the mean red level to adjust weights dynamically
		rMean := (r1 + c.R) / 2

		rDiff := r1 - c.R
		gDiff := g1 - c.G
		bDiff := b1 - c.B

		// "Redmean" perceptual distance approximation.
		// This heavily weights Green (which controls perceived luminosity) and
		// dynamically scales Red/Blue weighting based on how bright the red channel is.
		dist := (((512 + rMean) * rDiff * rDiff) >> 8) + (4 * gDiff * gDiff) + (((767 - rMean) * bDiff * bDiff) >> 8)

		if dist == 0 {
			return c.Code
		}

		if dist < minDist {
			minDist = dist
			bestCode = c.Code
		}
	}

	return bestCode
}

//nolint:funlen,gocyclo
func FormatCodeBlockText(text string, codeBlockBackTick bool, codeBlockTilde bool, lexer string, syntaxHighlighting string, linePrefix string) (string, bool, bool, string) {
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
			inlineCodeStart = inlineCode
			// Remove fence if not present
			if !strings.Contains(inlineCode, "`") {
				inlineCodeEnd = "\x11\x0f"
			}
		}
		// Not all IRC clients support monospace (0x11) so keep the fence
		msg = replaceCode(msg, inlineCodeStart, inlineCodeEnd)
	}

	// Block quotes
	trimmedText := strings.TrimLeft(msg, " \t")
	if strings.HasPrefix(trimmedText, blockQuoteCharDefault) && blockQuoteChar != blockQuoteCharDefault {
		var newPrefix strings.Builder
		idx := 0
	ParseLoop:
		for idx < len(trimmedText) {
			switch trimmedText[idx] {
			case '>':
				newPrefix.WriteString(blockQuoteChar)
				idx++
				// Markdown allows one optional space immediately after a '>'
				if idx < len(trimmedText) && trimmedText[idx] == ' ' {
					idx++
				}
			case ' ', '\t':
				// Skip any extra spaces between nested quote markers
				idx++
			default:
				break ParseLoop
			}
		}

		msg = newPrefix.String() + trimmedText[idx:]
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
