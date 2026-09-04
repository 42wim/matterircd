package utils

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2/quick"
)

// IRCColor represents a pre-parsed IRC color for fast distance calculations.
type IRCColor struct {
	Code    string
	R, G, B int
}

// ProcessMessageOpts holds the configuration for text processing
type ProcessMessageOpts struct {
	DisableEmoji bool
	CustomEmoji  map[string]string

	DisableMarkdown    bool
	SyntaxHighlighting string
	CodeBlockPrefix    string
	CodeBlockSeparator string
	BlockquoteChar     string
	InlineCodeChar     string

	PreserveNewLines string // "all" (default), "max-one", or "none" / "zero"
}

// SummaryOpts holds configuration for shortening and summarizing text.
type SummaryOpts struct {
	DisableEmoji bool
	CustomEmoji  map[string]string

	DisableMarkdown bool
	BlockquoteChar  string
	InlineCodeChar  string
	MaxLength       int
	UncountedPrefix string
	Unicode         bool
}

// Reuse bytes.Buffer for Chroma to prevent large chunk allocations
var chromaBufPool = sync.Pool{
	New: func() any {
		// Pre-allocate 1KB to handle most code blocks without slice growth
		return bytes.NewBuffer(make([]byte, 0, 1024))
	},
}

// Precalculate the palette on first access and cache the returned slice.
var getIRCPalette = sync.OnceValue(func() []IRCColor {
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

	var palette []IRCColor

	for i, hex := range rawColors {
		if seenHex[hex] {
			continue
		}

		seenHex[hex] = true

		r, _ := strconv.ParseInt(hex[0:2], 16, 32)
		g, _ := strconv.ParseInt(hex[2:4], 16, 32)
		b, _ := strconv.ParseInt(hex[4:6], 16, 32)

		palette = append(palette, IRCColor{
			Code: fmt.Sprintf("%02d", i),
			R:    int(r), G: int(g), B: int(b),
		})
	}

	return palette
})

// FindClosestIRCColor uses the precalculated palette to quickly find the nearest match.
func FindClosestIRCColor(hexColor string) string {
	palette := getIRCPalette()

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

	for _, c := range palette {
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

// FormatAndShortenSummary formats markdown/emoji, collapses newlines, and shortens text.
func FormatAndShortenSummary(text string, opts SummaryOpts) string {
	if text == "" {
		return ""
	}

	// Replace all newlines with space upfront to simplify processing.
	// strings.ReplaceAll is highly optimized and allocation-free if there's no match.
	text = strings.ReplaceAll(text, "\n", " ")

	text = FormatMarkdownAndEmoji(
		text,
		opts.DisableMarkdown,
		opts.DisableEmoji,
		opts.BlockquoteChar,
		opts.InlineCodeChar,
		opts.CustomEmoji,
	)

	if opts.MaxLength <= 0 || len(text) <= opts.MaxLength {
		return text
	}

	ellipsis := "..."
	if opts.Unicode {
		ellipsis = "…"
	}

	var b strings.Builder
	b.Grow(opts.MaxLength + 16)
	currentLimit := opts.MaxLength

	msg := text
	for len(msg) > 0 {
		if b.Len() >= currentLimit {
			break
		}

		idx := strings.IndexAny(msg, " \t")
		switch idx {
		case -1:
			processSummaryWord(&b, msg, opts.UncountedPrefix, ellipsis, &currentLimit)
			msg = ""
		case 0:
			if b.Len() < currentLimit {
				b.WriteByte(msg[0])
			}

			msg = msg[1:]
		default:
			processSummaryWord(&b, msg[:idx], opts.UncountedPrefix, ellipsis, &currentLimit)
			msg = msg[idx:]
		}
	}

	// Only append ellipsis if we actually cut the message short
	if len(msg) > 0 {
		b.WriteByte('\x0f')
		b.WriteByte(' ')
		b.WriteString(ellipsis)
	}

	return b.String()
}

// FormatFullCodeBlock handles syntax highlighting entirely in-memory and yields lines
//
//nolint:funlen
func FormatFullCodeBlock(text, lexer, indent string, opts ProcessMessageOpts, yield func(string)) {
	// Prepend the block's leading markdown indentation to the IRC prefix
	prefix := indent + opts.CodeBlockPrefix

	if text == "" {
		yield(prefix + " ")
		return
	}

	if lexer != "" {
		yield(prefix + "\x16" + lexer + "\x16")
	}

	if opts.SyntaxHighlighting == "" || lexer == "" {
		emitLines(text, prefix, yield)
		return
	}

	formatter, style := "terminal256", "pygments"
	if idx := strings.IndexByte(opts.SyntaxHighlighting, ':'); idx >= 0 {
		formatter = opts.SyntaxHighlighting[:idx]
		style = opts.SyntaxHighlighting[idx+1:]
	}

	bufInter := chromaBufPool.Get()
	buf, ok := bufInter.(*bytes.Buffer)

	if !ok {
		// Fallback just in case the pool returns something unexpected
		buf = bytes.NewBuffer(make([]byte, 0, 1024))
	}

	defer func() {
		// Prevent memory leaks from massive pasted code blocks.
		// If the buffer grew beyond 64KB, let the garbage collector claim it.
		if buf.Cap() <= 64*1024 {
			buf.Reset()
			chromaBufPool.Put(buf)
		}
	}()

	err := quick.Highlight(buf, text, lexer, formatter, style)
	if err == nil {
		bs := buf.Bytes()

		// Determine which reset sequence to look for based on the formatter
		resetSeq := "\x1b[0m" // Default for terminal* formatters
		if strings.HasPrefix(formatter, "mirc") {
			resetSeq = "\x0f"
		}

		hasReset := bytes.HasSuffix(bs, []byte(resetSeq))

		end := len(bs)
		if hasReset {
			end -= len(resetSeq)
		}

		// Work around https://github.com/alecthomas/chroma/issues/716
		// Safely strip the trailing newline without touching the linePrefix
		if end > 0 && bs[end-1] == '\n' {
			end--
			// Also safely handle \r\n just in case
			if end > 0 && bs[end-1] == '\r' {
				end--
			}
		}

		buf.Truncate(end)

		if hasReset {
			buf.WriteString(resetSeq)
		}

		emitLines(buf.String(), prefix, yield)

		return
	}

	// Fallback
	emitLines(text, prefix, yield)
}

// FormatMarkdownAndEmoji applies Markdown formatting and Emoji alias replacement.
// It utilizes a fast-path single-pass check to bypass processing entirely if no
// trigger characters are present, drastically reducing allocations and CPU usage.
func FormatMarkdownAndEmoji(msg string, disableMarkdown bool, disableEmoji bool, blockQuoteChar string, inlineCode string, customEmoji map[string]string) string {
	if !disableMarkdown {
		msg = Markdown2irc(msg, blockQuoteChar, inlineCode)
	}

	if !disableEmoji {
		msg = EmojiReplaceAliases(msg, customEmoji)
	}

	return msg
}

// ProcessMessageText abstracts the parsing loop, multi-line code handling,
// and formatting. It uses a zero-allocation callback (yield) to return lines.
//
//nolint:funlen,gocognit,gocyclo
func ProcessMessageText(text string, opts ProcessMessageOpts, yield func(line string)) {
	if text == "" {
		return
	}

	var (
		codeBuilder      strings.Builder
		emptyLines       int
		hasContent       bool
		lastBlockWasCode bool
		inCodeBlock      bool
		codeBlockMarker  string
		lexer            string
		currentIndent    string // Tracks leading whitespace for nested blocks
	)

	for text != "" {
		line, rest, _ := strings.Cut(text, "\n")
		text = rest

		origLine := line // Keep original to preserve \r and precise indentation
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)

		if inCodeBlock { //nolint:nestif
			if strings.HasPrefix(trimmed, codeBlockMarker) {
				inCodeBlock = false
				lastBlockWasCode = true
				hasContent = true

				FormatFullCodeBlock(codeBuilder.String(), lexer, currentIndent, opts, yield)
				codeBuilder.Reset() // Frees builder state for next block
			} else {
				if codeBuilder.Len() > 0 {
					codeBuilder.WriteByte('\n')
				}
				// Strip the structural leading whitespace to prevent double-indentation
				contentLine := origLine
				if currentIndent != "" && strings.HasPrefix(contentLine, currentIndent) {
					contentLine = contentLine[len(currentIndent):]
				}

				codeBuilder.WriteString(contentLine)
			}

			continue
		}

		isBacktick := strings.HasPrefix(trimmed, "```")
		isTilde := strings.HasPrefix(trimmed, "~~~")

		if isBacktick || isTilde { //nolint:nestif
			marker := "```"
			if isTilde {
				marker = "~~~"
			}

			// Capture the leading whitespace to use as indentation for the whole block
			idx := strings.Index(line, marker)
			if idx >= 0 {
				currentIndent = line[:idx]
			} else {
				currentIndent = ""
			}

			inCodeBlock = true
			codeBlockMarker = marker
			lexer = strings.TrimSpace(strings.TrimPrefix(trimmed, marker))

			// Logic for back-to-back code blocks
			if lastBlockWasCode && opts.CodeBlockSeparator != "" {
				yield(currentIndent + opts.CodeBlockSeparator)
			} else if !lastBlockWasCode {
				// Flush empty lines if they weren't between code blocks
				flushEmptyLines(emptyLines, opts.PreserveNewLines, hasContent, yield)
			}

			// Reset slice length without freeing memory
			emptyLines = 0

			codeBuilder.Grow(256)

			continue
		}

		if trimmed == "" {
			// Buffer empty lines
			emptyLines++
		} else {
			// Normal text line - flush buffered empty lines first
			flushEmptyLines(emptyLines, opts.PreserveNewLines, hasContent, yield)
			emptyLines = 0
			hasContent = true
			lastBlockWasCode = false

			line = FormatMarkdownAndEmoji(
				line,
				opts.DisableMarkdown,
				opts.DisableEmoji,
				opts.BlockquoteChar,
				opts.InlineCodeChar,
				opts.CustomEmoji,
			)

			yield(line)
		}
	}

	// Flush remaining state if EOF reached
	if inCodeBlock {
		FormatFullCodeBlock(codeBuilder.String(), lexer, currentIndent, opts, yield)
	} else if opts.PreserveNewLines == "all" || opts.PreserveNewLines == "" {
		for j := 0; j < emptyLines; j++ {
			yield("")
		}
	}
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

func emitLines(text, prefix string, yield func(string)) {
	for text != "" {
		line, rest, _ := strings.Cut(text, "\n")
		line = strings.TrimSuffix(line, "\r")

		// Skip allocation completely if no prefix is needed
		if prefix == "" {
			yield(line)
		} else {
			yield(prefix + line)
		}

		text = rest
	}
}

// isWordChar mimics ASCII \w (letters, digits, and underscores)
func isWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}

func processSummaryWord(b *strings.Builder, word, uncounted, ellipsis string, currentLimit *int) {
	if uncounted != "" && strings.HasPrefix(word, uncounted) {
		*currentLimit += len(word) + 1
		b.WriteString(word)

		return
	}

	if len(word) > *currentLimit {
		cut := min(*currentLimit*2/3, len(word))

		// UTF-8 boundary check to avoid splitting runes
		for cut > 0 && (word[cut]&0xC0) == 0x80 {
			cut--
		}

		b.WriteString(word[:cut])
		b.WriteByte('[')
		b.WriteString(ellipsis)
		b.WriteByte(']')
	} else {
		b.WriteString(word)
	}
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

// replaceLinks simulates the `\[([^\]]+)\]\(([^\)]+)\)` regex for Markdown links
func replaceLinks(msg string) string {
	var b strings.Builder

	start := 0
	i := 0

	for i < len(msg) {
		// Find opening bracket
		idx := strings.IndexByte(msg[i:], '[')
		if idx == -1 {
			break
		}

		absoluteIdx := i + idx

		// Find closing bracket
		closeBracketIdx := strings.IndexByte(msg[absoluteIdx+1:], ']')
		if closeBracketIdx == -1 {
			// No more complete brackets
			break
		}

		absoluteCloseBracketIdx := absoluteIdx + 1 + closeBracketIdx

		// Ensure '(' immediately follows ']'
		if absoluteCloseBracketIdx+1 >= len(msg) || msg[absoluteCloseBracketIdx+1] != '(' {
			// Not a markdown link, advance and continue searching
			i = absoluteCloseBracketIdx + 1
			continue
		}

		absoluteParenIdx := absoluteCloseBracketIdx + 1

		// Find closing parenthesis
		closeParenIdx := strings.IndexByte(msg[absoluteParenIdx+1:], ')')
		if closeParenIdx == -1 {
			i = absoluteParenIdx + 1
			continue
		}

		absoluteCloseParenIdx := absoluteParenIdx + 1 + closeParenIdx

		// Valid link found, lazy allocation
		if start == 0 {
			b.Grow(len(msg) + 32)
		}

		b.WriteString(msg[start:absoluteIdx])

		// Underline the text: \x1F + text + \x1F
		b.WriteString("\x1f")
		b.WriteString(msg[absoluteIdx+1 : absoluteCloseBracketIdx])
		b.WriteString("\x1f ") // Close underline and add the space

		// Italicize the URL with parenthesis: (\x1D + url + \x1D)
		b.WriteString("(\x1d")
		b.WriteString(msg[absoluteParenIdx+1 : absoluteCloseParenIdx])
		b.WriteString("\x1d)")

		// Move start pointer past the matched link
		start = absoluteCloseParenIdx + 1
		i = start
	}

	if start == 0 {
		return msg // Zero allocations if no valid links were found
	}

	b.WriteString(msg[start:])

	return b.String()
}

const BlockQuoteCharDefault = ">"

func Markdown2irc(msg string, blockQuoteChar string, inlineCode string) string {
	if !strings.ContainsAny(msg, "*_`>~[") {
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

	// Strikethrough 0x1E processing
	if strings.Contains(msg, "~") {
		msg = replacePattern(msg, "~~", "\x1e", "\x1e", false)
	}

	// Code / Monospace 0x11 processing
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

	// Links processing [text](url)
	if strings.Contains(msg, "[") {
		msg = replaceLinks(msg)
	}

	// Block quotes
	trimmedText := strings.TrimLeft(msg, " \t")
	if strings.HasPrefix(trimmedText, BlockQuoteCharDefault) && blockQuoteChar != BlockQuoteCharDefault {
		var newPrefix strings.Builder
		newPrefix.Grow(len(trimmedText)) // Pre-allocate exactly what we need

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

		// Zero-allocation write of the remaining string
		newPrefix.WriteString(trimmedText[idx:])
		msg = newPrefix.String()
	}

	return msg
}

// WrapMessage soft-wraps msg into lines of at most maxLen bytes, breaking
// only at spaces/newlines. Words longer than maxLen are left unbroken and
// overflow their line rather than being split - fine here since IRC's real
// line limit (512) leaves headroom above maxLen (460), and splitting mid-
// word would corrupt URLs/tokens. Pure byte scanning throughout
//
//nolint:gocyclo
func WrapMessage(msg string, maxLen int) string {
	if maxLen <= 0 || len(msg) <= maxLen {
		return msg
	}

	var (
		b           strings.Builder
		initialized bool
		lastWrite   int
	)

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
			// Natural newline resets line length, no need to wrap
			lineLen, spaceLen = 0, 0
			i++

		default:
			start := i
			for i < len(msg) && msg[i] != ' ' && msg[i] != '\t' && msg[i] != '\n' {
				i++
			}

			wordLen := i - start

			// If this word pushes us over the limit, we need to inject a newline
			if lineLen > 0 && lineLen+spaceLen+wordLen > maxLen {
				if !initialized {
					b.Grow(len(msg) + (len(msg)/maxLen)*2)

					initialized = true
				}

				// Write everything from our last position up to the spaces we are splitting on
				b.WriteString(msg[lastWrite:spaceStart])
				b.WriteByte('\n') // Inject the wrap

				// Skip over the spaces we just wrapped on
				lastWrite = spaceStart + spaceLen

				lineLen = wordLen
				spaceLen = 0
			} else {
				lineLen += spaceLen + wordLen
				spaceLen = 0
			}
		}
	}

	// Fast path: no wraps were ever required, return original string
	if !initialized {
		return msg
	}

	// Flush whatever remains of the string
	b.WriteString(msg[lastWrite:])
	return b.String()
}

func flushEmptyLines(count int, mode string, hasContent bool, yield func(line string)) {
	if count == 0 {
		return
	}

	switch mode {
	case "none", "zero":
		return

	case "max-one":
		if hasContent {
			yield("")
		}

	case "all":
		fallthrough
	default:
		for j := 0; j < count; j++ {
			yield("")
		}
	}
}
