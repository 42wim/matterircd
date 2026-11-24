package utils

import (
	"bytes"
	"fmt"
	"math"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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
//nolint:funlen,gocyclo
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
	if err == nil { //nolint:nestif
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

// Irc2Markdown converts IRC formatting back to Markdown, handling links and text styles.
//
//nolint:funlen,gocyclo
func Irc2Markdown(msg string) string {
	if !strings.ContainsAny(msg, "\x02\x1d\x1e\x11\x1f\x03\x04\x0f\x16") {
		return msg
	}

	if strings.Contains(msg, "\x03") {
		msg = stripIrcColors(msg)
	}

	if strings.Contains(msg, "\x04") {
		msg = stripHexColors(msg)
	}

	if strings.Contains(msg, "\x1f") {
		msg = reverseLinks(msg)
	}

	// https://modern.ircdocs.horse/formatting.html
	emphasisSupported := map[byte]string{
		'\x02': "**", // Bold      0x02
		'\x1d': "_",  // Italics   0x1D
		'\x1e': "~~", // Strikethr 0x1E
		'\x11': "`",  // Monospace 0x11
	}

	emphasisUnsupported := map[byte]struct{}{
		'\x1f': {}, // Underline 0x1F (stripped if not consumed by reverseLinks)
		'\x16': {}, // Reverse Color
	}

	// Fast path exit if only colors/links were present and stripped
	if !strings.ContainsAny(msg, "\x02\x1d\x11\x0f\x1f\x1e\x16") {
		return msg
	}

	var b strings.Builder

	var currentEmphasis []byte

	b.Grow(len(msg) + 16)

	for i := range len(msg) {
		char := msg[i]

		// Strip or ignore unsuppored IRC formatting / emphasis
		if _, ok := emphasisUnsupported[char]; ok {
			continue
		}

		// Not an IRC formatting / emphasis character so copy as is
		if emp, ok := emphasisSupported[char]; !ok {
			// IRC reset so reset formatting
			if char == '\x0f' {
				// Close off any current formatting / emphasis
				for _, c := range currentEmphasis {
					b.WriteString(emphasisSupported[c])
				}

				currentEmphasis = nil

				continue
			}

			b.WriteByte(char)

			continue
		} else {
			// Supported formatting character
			b.WriteString(emp)

			var newEmphasis []byte

			// Closing emphasis, they're in pairs, remove for list of outstanding
			found := false
			for _, c := range currentEmphasis {
				if !found && c == char {
					found = true
					continue
				}

				newEmphasis = append(newEmphasis, c)
			}

			if found {
				currentEmphasis = newEmphasis
				continue
			}

			// Otherwise, open a new emphasis
			currentEmphasis = append([]byte{char}, currentEmphasis...)
		}
	}

	// Close off any current formatting / emphasis
	for _, c := range currentEmphasis {
		b.WriteString(emphasisSupported[c])
	}

	return b.String()
}

// reverseLinks converts IRC formatted links \x1ftext\x1f (url) back to [text](url)
//
//nolint:funlen
func reverseLinks(msg string) string {
	var b strings.Builder

	start := 0
	i := 0

	for i < len(msg) {
		idx := strings.IndexByte(msg[i:], '\x1f')
		if idx == -1 {
			break
		}

		absIdx := i + idx

		closeIdx := strings.IndexByte(msg[absIdx+1:], '\x1f')
		if closeIdx == -1 {
			break
		}

		absCloseIdx := absIdx + 1 + closeIdx

		parenStartOffset := strings.IndexByte(msg[absCloseIdx+1:], '(')
		if parenStartOffset == -1 {
			i = absCloseIdx + 1

			continue
		}

		absParenStart := absCloseIdx + 1 + parenStartOffset

		// Verify only spaces or non-breaking spaces (\u00a0) exist between \x1f and (
		between := msg[absCloseIdx+1 : absParenStart]
		isSpace := true

		for _, r := range between {
			if r != ' ' && r != '\u00a0' {
				isSpace = false

				break
			}
		}

		if !isSpace {
			i = absCloseIdx + 1

			continue
		}

		parenEndOffset := strings.IndexByte(msg[absParenStart+1:], ')')
		if parenEndOffset == -1 {
			i = absParenStart + 1

			continue
		}

		absParenEnd := absParenStart + 1 + parenEndOffset

		if start == 0 {
			b.Grow(len(msg))
		}

		b.WriteString(msg[start:absIdx])

		text := msg[absIdx+1 : absCloseIdx]
		url := msg[absParenStart+1 : absParenEnd]

		// Clean up italics from the URL if the IRC client moved them inside the parens
		url = strings.ReplaceAll(url, "\x1d", "")

		b.WriteString("[")
		b.WriteString(text)
		b.WriteString("](")
		b.WriteString(url)
		b.WriteString(")")

		start = absParenEnd + 1
		i = start
	}

	if start == 0 {
		return msg
	}

	b.WriteString(msg[start:])

	return b.String()
}

// stripIrcColors removes \x03 and following numbers/commas without allocations
func stripIrcColors(msg string) string {
	idx := strings.IndexByte(msg, '\x03')
	if idx == -1 {
		return msg
	}

	var b strings.Builder

	b.Grow(len(msg))

	start := 0
	i := idx

	for i < len(msg) {
		if msg[i] != '\x03' {
			i++

			continue
		}

		b.WriteString(msg[start:i])

		i++

		// Skip up to 2 foreground digits
		digitCount := 0

		for i < len(msg) && digitCount < 2 && msg[i] >= '0' && msg[i] <= '9' {
			i++

			digitCount++
		}

		// Skip comma and up to 2 background digits
		if i < len(msg) && msg[i] == ',' {
			i++

			digitCount = 0

			for i < len(msg) && digitCount < 2 && msg[i] >= '0' && msg[i] <= '9' {
				i++

				digitCount++
			}
		}

		start = i
	}

	b.WriteString(msg[start:])

	return b.String()
}

// stripHexColors removes \x04RRGGBB hex color codes without allocations
func stripHexColors(msg string) string {
	idx := strings.IndexByte(msg, '\x04')
	if idx == -1 {
		return msg
	}

	var b strings.Builder

	b.Grow(len(msg))

	start := 0
	i := idx

	for i < len(msg) {
		if msg[i] != '\x04' {
			i++

			continue
		}

		b.WriteString(msg[start:i])

		i++

		hexCount := 0

		for i < len(msg) && hexCount < 6 {
			c := msg[i]
			if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
				i++

				hexCount++
			} else {
				break
			}
		}

		start = i
	}

	b.WriteString(msg[start:])

	return b.String()
}

const (
	AttachmentMarkerStart = "\x00<ATTM>\x00"
	AttachmentMarkerEnd   = "\x00</ATTM>\x00"
)

// ProcessMessageText abstracts the parsing loop, multi-line code handling,
// and formatting. It uses a zero-allocation callback (yield) to return lines.
//
//nolint:funlen,gocognit,gocyclo
func ProcessMessageText(text string, opts ProcessMessageOpts, yield func(line string)) {
	if text == "" {
		return
	}

	var (
		codeBuilder       strings.Builder
		emptyLines        int
		hasContent        bool
		lastBlockWasCode  bool
		inCodeBlock       bool
		inQuotedCodeBlock bool
		inAttachment      bool
		codeBlockMarker   string
		lexer             string
		currentIndent     string // Tracks leading whitespace for nested blocks
		tbl               tableParser
	)

	emitLine := func(l string) {
		flushEmptyLines(emptyLines, opts.PreserveNewLines, hasContent, yield)
		emptyLines = 0
		hasContent = true
		lastBlockWasCode = false

		yield(FormatMarkdownAndEmoji(l, opts.DisableMarkdown, opts.DisableEmoji, opts.BlockquoteChar, opts.InlineCodeChar, opts.CustomEmoji))
	}

	flushTable := func() {
		if tbl.flush(opts, emitLine, yield) {
			hasContent = true
			lastBlockWasCode = false
		}
	}

	for text != "" {
		line, rest, _ := strings.Cut(text, "\n")
		text = rest

		origLine := line // Keep original to preserve \r and precise indentation
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)

		if strings.Contains(line, AttachmentMarkerStart) {
			flushTable()

			line = strings.ReplaceAll(line, AttachmentMarkerStart, "")
			inAttachment = true
			trimmed = strings.TrimSpace(line)

			// If the line was solely the marker, strip it completely
			if trimmed == "" {
				continue
			}
		}

		wasInAttachment := inAttachment

		if strings.Contains(line, AttachmentMarkerEnd) {
			line = strings.ReplaceAll(line, AttachmentMarkerEnd, "")
			inAttachment = false
			trimmed = strings.TrimSpace(line)

			if trimmed == "" {
				continue
			}
		}

		// Bypass Markdown/Emoji parsing for lines originating from message attachments
		if inAttachment || wasInAttachment {
			flushTable()

			if trimmed == "" {
				emptyLines++
			} else {
				flushEmptyLines(emptyLines, opts.PreserveNewLines, hasContent, yield)
				emptyLines = 0
				hasContent = true
				lastBlockWasCode = false

				yield(line)
			}

			continue
		}

		if inCodeBlock { //nolint:nestif
			if inQuotedCodeBlock {
				if _, qrest, ok := splitQuotePrefix(line, opts.BlockquoteChar, opts.InlineCodeChar); ok {
					line = qrest
					origLine = qrest
					trimmed = strings.TrimSpace(line)
				}
			}

			isPrefixClose := strings.HasPrefix(trimmed, codeBlockMarker)

			closeIdx := -1
			if isPrefixClose {
				closeIdx = strings.Index(line, codeBlockMarker)
			} else if idx := strings.LastIndex(line, codeBlockMarker); idx >= 0 {
				after := strings.TrimSpace(line[idx+len(codeBlockMarker):])
				if after == "" || strings.HasPrefix(after, "\x1d(") {
					closeIdx = idx
				}
			}

			if closeIdx >= 0 {
				if !isPrefixClose && strings.TrimSpace(line[:closeIdx]) != "" {
					contentBefore := origLine
					if idxOrig := strings.LastIndex(contentBefore, codeBlockMarker); idxOrig >= 0 {
						contentBefore = contentBefore[:idxOrig]
					}

					if currentIndent != "" && strings.HasPrefix(contentBefore, currentIndent) {
						contentBefore = contentBefore[len(currentIndent):]
					}

					if codeBuilder.Len() > 0 {
						codeBuilder.WriteByte('\n')
					}

					codeBuilder.WriteString(contentBefore)
				}

				inCodeBlock = false
				inQuotedCodeBlock = false
				lastBlockWasCode = true
				hasContent = true

				FormatFullCodeBlock(codeBuilder.String(), lexer, currentIndent, opts, yield)
				codeBuilder.Reset() // Frees builder state for next block

				trailing := strings.TrimSpace(line[closeIdx+len(codeBlockMarker):])
				if trailing != "" {
					lastBlockWasCode = false
					trailing = FormatMarkdownAndEmoji(
						trailing,
						opts.DisableMarkdown,
						opts.DisableEmoji,
						opts.BlockquoteChar,
						opts.InlineCodeChar,
						opts.CustomEmoji,
					)

					yield(trailing)
				}
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

		if tbl.consume(line, trimmed, opts, emitLine, flushTable, func() {
			flushEmptyLines(emptyLines, opts.PreserveNewLines, hasContent, yield)
			emptyLines = 0
		}) {
			continue
		}

		formattedQuotePrefix := ""
		fenceTrimmed := trimmed

		if !opts.DisableMarkdown {
			if qp, qrest, ok := splitQuotePrefix(line, opts.BlockquoteChar, opts.InlineCodeChar); ok {
				formattedQuotePrefix = qp
				fenceTrimmed = strings.TrimSpace(qrest)
			}
		}

		isBacktick := strings.HasPrefix(fenceTrimmed, "```")
		isTilde := strings.HasPrefix(fenceTrimmed, "~~~")

		if isBacktick || isTilde { //nolint:nestif
			marker := "```"
			if isTilde {
				marker = "~~~"
			}

			if formattedQuotePrefix != "" {
				inQuotedCodeBlock = true
				currentIndent = formattedQuotePrefix
			} else {
				inQuotedCodeBlock = false
				// Capture the leading whitespace to use as indentation for the whole block
				idx := strings.Index(line, marker)
				if idx >= 0 {
					currentIndent = line[:idx]
				} else {
					currentIndent = ""
				}
			}

			inCodeBlock = true
			codeBlockMarker = marker
			lexer = strings.TrimSpace(strings.TrimPrefix(fenceTrimmed, marker))

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
		} else if !tbl.tryStart(line, opts) {
			emitLine(line)
		}
	}

	// Flush remaining state if EOF reached
	flushTable()

	if inCodeBlock {
		FormatFullCodeBlock(codeBuilder.String(), lexer, currentIndent, opts, yield)
	} else if opts.PreserveNewLines == "all" || opts.PreserveNewLines == "" {
		for range emptyLines {
			yield("")
		}
	}
}

func splitQuotePrefix(line, blockQuoteChar, inlineCodeChar string) (string, string, bool) {
	trimmedText := strings.TrimLeft(line, " \t")

	if blockQuoteChar != "" && blockQuoteChar != BlockQuoteCharDefault && strings.HasPrefix(trimmedText, blockQuoteChar) {
		var pre strings.Builder

		idx := 0
		for strings.HasPrefix(trimmedText[idx:], blockQuoteChar) {
			pre.WriteString(blockQuoteChar)
			idx += len(blockQuoteChar)

			if idx < len(trimmedText) && trimmedText[idx] == ' ' {
				idx++
			}
		}

		return pre.String(), trimmedText[idx:], true
	}

	if !strings.HasPrefix(trimmedText, BlockQuoteCharDefault) {
		return "", line, false
	}

	leading := len(line) - len(trimmedText)
	idx := 0

ParseLoop:
	for idx < len(trimmedText) {
		switch trimmedText[idx] {
		case '>':
			idx++
			if idx < len(trimmedText) && trimmedText[idx] == ' ' {
				idx++
			}
		case ' ', '\t':
			idx++
		default:
			break ParseLoop
		}
	}

	bq := blockQuoteChar
	if bq == "" {
		bq = BlockQuoteCharDefault
	}

	return Markdown2irc(line[:leading+idx], bq, inlineCodeChar), trimmedText[idx:], true
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

const (
	BlockQuoteCharDefault = ">"
	horizontalRuleLine    = "────────────────────"
)

type tableAlign uint8

const (
	tableAlignLeft tableAlign = iota
	tableAlignCenter
	tableAlignRight
)

var unorderedListBullets = [3]string{"•", "◦", "▪"}

func isASCIIPunctuation(c byte) bool {
	return (c >= '!' && c <= '/') ||
		(c >= ':' && c <= '@') ||
		(c >= '[' && c <= '`') ||
		(c >= '{' && c <= '~')
}

func inlineCodeDelims(inlineCode string) (string, string) {
	inlineCodeStart := "\x0f`\x11\x02\x030,14"
	inlineCodeEnd := "\x11\x0f`"

	if inlineCode != "" {
		inlineCodeStart = inlineCode
		// Remove fence if not present
		if !strings.Contains(inlineCode, "`") {
			inlineCodeEnd = "\x11\x0f"
		}
	}

	return inlineCodeStart, inlineCodeEnd
}

func appendToken(b *strings.Builder, tokens *[]string, totalLen int, prefix, tok string) {
	if len(*tokens) == 0 {
		b.Grow(totalLen + 32)
	}

	b.WriteString(prefix)

	id := len(*tokens)
	*tokens = append(*tokens, tok)

	b.WriteByte('\x00')
	b.WriteString(strconv.Itoa(id))
	b.WriteByte('\x00')
}

// parseCodeSpan parses a CommonMark inline code span starting at msg[i] == '`',
// returning the inner code, the index after the closing backticks, and whether a match was found.
func parseCodeSpan(msg string, i int) (string, int, bool) {
	runLen := 1
	for i+runLen < len(msg) && msg[i+runLen] == '`' {
		runLen++
	}

	for pos := i + runLen; pos < len(msg); {
		idx := strings.IndexByte(msg[pos:], '`')
		if idx == -1 {
			break
		}

		pos += idx
		end := pos + 1

		for end < len(msg) && msg[end] == '`' {
			end++
		}

		if end-pos == runLen {
			code := msg[i+runLen : pos]
			if len(code) >= 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
				code = code[1 : len(code)-1]
			}

			return code, end, true
		}

		pos = end
	}

	return "", i + runLen, false
}

// tokenizeCodeAndEscapes replaces inline code spans (`...`), backslash-escaped
// ASCII punctuation (\*), and literal NUL bytes with indexed placeholders.
func tokenizeCodeAndEscapes(msg, inlineCode string) (string, []string) {
	if !strings.ContainsAny(msg, "`\\\x00") {
		return msg, nil
	}

	var (
		b                  strings.Builder
		tokens             []string
		codeStart, codeEnd string
		start, i           int
	)

	for i < len(msg) {
		switch msg[i] {
		case '\x00':
			appendToken(&b, &tokens, len(msg), msg[start:i], "\x00")

			i++
			start = i
		case '\\':
			if i+1 < len(msg) && isASCIIPunctuation(msg[i+1]) {
				appendToken(&b, &tokens, len(msg), msg[start:i], msg[i+1:i+2])

				i += 2
				start = i

				continue
			}

			i++
		case '`':
			code, nextIdx, ok := parseCodeSpan(msg, i)
			if !ok {
				i = nextIdx

				continue
			}

			if codeStart == "" {
				codeStart, codeEnd = inlineCodeDelims(inlineCode)
			}

			appendToken(&b, &tokens, len(msg), msg[start:i], codeStart+code+codeEnd)

			i = nextIdx
			start = i
		default:
			i++
		}
	}

	if len(tokens) == 0 {
		return msg, nil
	}

	b.WriteString(msg[start:])

	return b.String(), tokens
}

func findMatchingParen(s string, start int) int {
	depth := 1

	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

func writeLinkOrImage(b *strings.Builder, linkText string, urlID int, isImage bool) {
	b.WriteByte('\x1f')

	switch {
	case isImage && linkText != "":
		b.WriteString("[image: ")
		b.WriteString(linkText)
		b.WriteByte(']')
	case isImage:
		b.WriteString("[image]")
	default:
		b.WriteString(linkText)
	}

	b.WriteString("\x1f\u00a0(\x1d\x00")
	b.WriteString(strconv.Itoa(urlID))
	b.WriteString("\x00\x1d)")
}

// replaceLinksAndImages converts Markdown links [text](url) and images ![alt](url),
// storing URLs in tokens so subsequent emphasis passes do not mangle them.
//
//nolint:funlen
func replaceLinksAndImages(msg string, tokens *[]string) string {
	var b strings.Builder

	start, i := 0, 0

	for i < len(msg) {
		idx := strings.IndexByte(msg[i:], '[')
		if idx == -1 {
			break
		}

		openBracket := i + idx

		closeOff := strings.IndexByte(msg[openBracket+1:], ']')
		if closeOff == -1 {
			break
		}

		closeBracket := openBracket + 1 + closeOff

		if inner := strings.IndexByte(msg[openBracket+1:closeBracket], '['); inner >= 0 {
			i = openBracket + 1 + inner

			continue
		}

		if closeBracket+1 >= len(msg) || msg[closeBracket+1] != '(' {
			i = closeBracket + 1

			continue
		}

		closeParen := findMatchingParen(msg, closeBracket+2)
		if closeParen == -1 {
			i = closeBracket + 2

			continue
		}

		isImage := openBracket > start && msg[openBracket-1] == '!'
		linkText := msg[openBracket+1 : closeBracket]
		rawURL := msg[closeBracket+2 : closeParen]

		if rawURL == "" || (!isImage && linkText == "") {
			i = closeParen + 1

			continue
		}

		matchStart := openBracket
		if isImage {
			matchStart--
		}

		if start == 0 {
			b.Grow(len(msg) + 32)
		}

		b.WriteString(msg[start:matchStart])

		urlID := len(*tokens)
		*tokens = append(*tokens, restoreTokens(rawURL, *tokens, "", ""))

		writeLinkOrImage(&b, linkText, urlID, isImage)

		start = closeParen + 1
		i = start
	}

	if start == 0 {
		return msg
	}

	b.WriteString(msg[start:])

	return b.String()
}

// restoreTokens expands \x00<id>\x00 placeholders, optionally wrapping runs
// between inline code resets (\x0f) in open/closeCode IRC control codes.
//
//nolint:funlen,gocyclo
func restoreTokens(s string, tokens []string, open, closeCode string) string {
	if s == "" || len(tokens) == 0 || !strings.ContainsRune(s, '\x00') {
		if s == "" {
			return ""
		}

		return open + s + closeCode
	}

	var b strings.Builder
	b.Grow(len(s) + len(open) + len(closeCode) + 32)

	styled := false
	writeChunk := func(chunk string) {
		if chunk == "" {
			return
		}

		if open != "" && !styled {
			b.WriteString(open)

			styled = true
		}

		b.WriteString(chunk)
	}

	for i := 0; i < len(s); {
		idx := strings.IndexByte(s[i:], '\x00')
		if idx == -1 {
			writeChunk(s[i:])

			break
		}

		writeChunk(s[i : i+idx])

		pos := i + idx

		endOff := strings.IndexByte(s[pos+1:], '\x00')
		if endOff == -1 {
			writeChunk(s[pos:])

			break
		}

		nextIdx := pos + endOff + 2

		id, err := strconv.Atoi(s[pos+1 : pos+1+endOff])
		if err != nil || id < 0 || id >= len(tokens) {
			writeChunk(s[pos:nextIdx])
		} else if tok := tokens[id]; open != "" && strings.ContainsRune(tok, '\x0f') {
			b.WriteString(tok)

			styled = false
		} else {
			writeChunk(tok)
		}

		i = nextIdx
	}

	if styled {
		b.WriteString(closeCode)
	}

	return b.String()
}

func formatInlineMarkdown(msg, inlineCode string, bold bool) string {
	if msg == "" {
		return msg
	}

	if !bold && !strings.ContainsAny(msg, "*_`~[\\") {
		return msg
	}

	work, tokens := tokenizeCodeAndEscapes(msg, inlineCode)

	if strings.ContainsRune(work, '[') {
		work = replaceLinksAndImages(work, &tokens)
	}

	boldItalicStart, boldItalicEnd := "\x02\x1d", "\x1d\x02"
	boldStart, boldEnd := "\x02", "\x02"

	if bold {
		boldItalicStart, boldItalicEnd = "\x1d", "\x1d"
		boldStart, boldEnd = "", ""
	}

	if strings.ContainsRune(work, '*') {
		work = replacePattern(work, "***", boldItalicStart, boldItalicEnd, false)
		work = replacePattern(work, "**", boldStart, boldEnd, false)
		work = replacePattern(work, "*", "\x1d", "\x1d", false)
	}

	if strings.ContainsRune(work, '_') {
		work = replacePattern(work, "___", boldItalicStart, boldItalicEnd, true)
		work = replacePattern(work, "__", boldStart, boldEnd, true)
		work = replacePattern(work, "_", "\x1d", "\x1d", true)
	}

	if strings.ContainsRune(work, '~') {
		work = replacePattern(work, "~~", "\x1e", "\x1e", false)
	}

	if bold {
		return restoreTokens(work, tokens, "\x02", "\x02")
	}

	return restoreTokens(work, tokens, "", "")
}

func trimLeadingSpacesMax3(line string) (string, bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
		if i > 3 {
			return "", false
		}
	}

	if i >= len(line) {
		return "", false
	}

	return line[i:], true
}

// isHorizontalRule checks for CommonMark thematic breaks (3+ matching '-', '*', or '_'
// with optional spaces/tabs and up to 3 leading spaces).
func isHorizontalRule(line string) bool {
	rest, ok := trimLeadingSpacesMax3(line)
	if !ok {
		return false
	}

	marker := rest[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}

	count := 0

	for i := range len(rest) {
		switch rest[i] {
		case marker:
			count++
		case ' ', '\t':
			// allowed between or after markers
		default:
			return false
		}
	}

	return count >= 3
}

// parseHeading parses a CommonMark ATX heading (# through ###### followed by space/tab).
func parseHeading(line string) (int, string, bool) {
	rest, ok := trimLeadingSpacesMax3(line)
	if !ok || rest[0] != '#' {
		return 0, "", false
	}

	hashes := 0
	for hashes < len(rest) && rest[hashes] == '#' {
		hashes++
	}

	if hashes > 6 || hashes >= len(rest) || (rest[hashes] != ' ' && rest[hashes] != '\t') {
		return 0, "", false
	}

	content := strings.TrimSpace(rest[hashes:])
	if content == "" {
		return 0, "", false
	}

	// Strip optional CommonMark closing '#' sequence preceded by space/tab
	trimmedRight := strings.TrimRight(content, "#")
	if len(trimmedRight) < len(content) && (trimmedRight == "" || trimmedRight[len(trimmedRight)-1] == ' ' || trimmedRight[len(trimmedRight)-1] == '\t') {
		content = strings.TrimSpace(trimmedRight)
	}

	return hashes, content, content != ""
}

func isNonTableBlock(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		return true
	}

	_, _, ok := parseHeading(line)

	return ok
}

// parseUnorderedList parses a Markdown unordered list item (-, *, + followed by space/tab)
// and normalizes the marker to a bullet character based on nesting depth.
func parseUnorderedList(line string) (string, string, bool) {
	i, indentWidth := 0, 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		if line[i] == '\t' {
			indentWidth += 2
		} else {
			indentWidth++
		}

		i++
	}

	if i+1 >= len(line) || (line[i] != '-' && line[i] != '*' && line[i] != '+') || (line[i+1] != ' ' && line[i+1] != '\t') {
		return "", "", false
	}

	rest := strings.TrimLeft(line[i+1:], " \t")
	if rest == "" {
		return "", "", false
	}

	bullet := unorderedListBullets[(indentWidth/2)%len(unorderedListBullets)]

	return line[:i] + bullet + " ", rest, true
}

func skipInlineSpace(s string, idx int) int {
	for idx < len(s) && (s[idx] == ' ' || s[idx] == '\t') {
		idx++
	}

	return idx
}

// parseMarkdownQuotePrefix splits and formats any leading blockquote prefix on msg.
// It supports both raw '>' markers and pre-replaced blockQuoteChar prefixes.
//
//nolint:funlen,gocyclo
func parseMarkdownQuotePrefix(msg, blockQuoteChar string) (string, string, bool, bool) {
	trimmedText := strings.TrimLeft(msg, " \t")
	if trimmedText == "" {
		return "", msg, false, false
	}

	hasCustomBQ := blockQuoteChar != "" && blockQuoteChar != BlockQuoteCharDefault
	isPreReplaced := hasCustomBQ && strings.HasPrefix(trimmedText, blockQuoteChar) &&
		(blockQuoteChar != "|" || !strings.ContainsRune(trimmedText[len(blockQuoteChar):], '|'))

	if !isPreReplaced && trimmedText[0] != '>' {
		return "", msg, false, false
	}

	var pre strings.Builder

	idx := 0
	changed := false

	if isPreReplaced {
		for strings.HasPrefix(trimmedText[idx:], blockQuoteChar) {
			pre.WriteString(blockQuoteChar)
			idx += len(blockQuoteChar)

			next := skipInlineSpace(trimmedText, idx)
			if strings.HasPrefix(trimmedText[next:], blockQuoteChar) || (next < len(trimmedText) && trimmedText[next] == '>') {
				idx = next

				continue
			}

			if idx < len(trimmedText) && trimmedText[idx] == ' ' {
				pre.WriteByte(' ')

				idx++
			}

			break
		}
	}

	for idx < len(trimmedText) && trimmedText[idx] == '>' {
		if hasCustomBQ {
			pre.WriteString(blockQuoteChar)

			changed = true
		}

		idx++
		if idx < len(trimmedText) && trimmedText[idx] == ' ' {
			idx++
		}

		if next := skipInlineSpace(trimmedText, idx); next < len(trimmedText) && trimmedText[next] == '>' {
			idx = next
		}
	}

	if !isPreReplaced && !hasCustomBQ {
		return msg[:len(msg)-len(trimmedText)+idx], trimmedText[idx:], true, false
	}

	return pre.String(), trimmedText[idx:], true, changed
}

func hasMarkdownBlockTrigger(trimmedStart, blockQuoteChar string) bool {
	if trimmedStart == "" {
		return false
	}

	switch trimmedStart[0] {
	case '#', '-', '+', '>', '*', '_':
		return true
	}

	return blockQuoteChar != "" && blockQuoteChar != BlockQuoteCharDefault && strings.HasPrefix(trimmedStart, blockQuoteChar)
}

func Markdown2irc(msg string, blockQuoteChar string, inlineCode string) string {
	if msg == "" {
		return msg
	}

	trimmedStart := strings.TrimLeft(msg, " \t")
	if !hasMarkdownBlockTrigger(trimmedStart, blockQuoteChar) && !strings.ContainsAny(msg, "*_`~[\\") {
		return msg
	}

	quotePrefix, content, hasQuote, quoteChanged := parseMarkdownQuotePrefix(msg, blockQuoteChar)
	if !hasQuote {
		content = msg
	}

	// Horizontal rule (must precede unordered list since "* * *" and "- - -" match both)
	if isHorizontalRule(content) {
		return quotePrefix + horizontalRuleLine
	}

	// ATX Heading (# through ######): wrapped in '=' per level, bold text for H1-H2 only
	if level, headingText, ok := parseHeading(content); ok {
		marker := strings.Repeat("=", level)
		formatted := formatInlineMarkdown(headingText, inlineCode, level <= 2)

		return quotePrefix + marker + " " + formatted + " " + marker
	}

	// Unordered list (-, *, +)
	if listPrefix, itemText, ok := parseUnorderedList(content); ok {
		formatted := formatInlineMarkdown(itemText, inlineCode, false)

		return quotePrefix + listPrefix + formatted
	}

	formatted := formatInlineMarkdown(content, inlineCode, false)
	if !hasQuote {
		return formatted
	}

	if !quoteChanged && formatted == content {
		return msg
	}

	return quotePrefix + formatted
}

type tableParser struct {
	hasPending  bool
	pendingLine string
	active      bool
	header      []string
	aligns      []tableAlign
	body        [][]string
	prefix      string
	quoted      bool
}

func (p *tableParser) flush(opts ProcessMessageOpts, emitLine func(string), yield func(string)) bool {
	if p.hasPending {
		line := p.pendingLine
		*p = tableParser{}

		emitLine(line)

		return false
	}

	if !p.active {
		return false
	}

	header, aligns, body, prefix := p.header, p.aligns, p.body, p.prefix
	*p = tableParser{}

	formatTable(header, aligns, body, prefix, opts, yield)

	return true
}

func (p *tableParser) tryStart(line string, opts ProcessMessageOpts) bool {
	if opts.DisableMarkdown || !strings.ContainsRune(line, '|') {
		return false
	}

	prefix, content, quoted := extractTableLine(line, opts)
	if isNonTableBlock(content) || isHorizontalRule(content) {
		return false
	}

	cells, ok := splitTableRow(content)
	if !ok {
		return false
	}

	p.hasPending = true
	p.pendingLine = line
	p.header = cells
	p.prefix = prefix
	p.quoted = quoted

	return true
}

func (p *tableParser) consume(line, trimmed string, opts ProcessMessageOpts, emitLine func(string), flushTable, flushEmpty func()) bool {
	if !p.hasPending && !p.active {
		return false
	}

	if trimmed == "" {
		flushTable()

		return false
	}

	tPrefix, tContent, tQuoted := extractTableLine(line, opts)
	if tQuoted != p.quoted || (tQuoted && tPrefix != p.prefix) {
		flushTable()

		return false
	}

	rowPart, suffix := stripTableTrailingSuffix(tContent)

	if p.hasPending {
		aligns, ok := parseTableDelimiterRow(rowPart, len(p.header))
		if !ok {
			flushTable()

			return false
		}

		flushEmpty()

		p.hasPending = false
		p.pendingLine = ""
		p.active = true
		p.aligns = aligns

		if suffix != "" {
			flushTable()
			emitLine(suffix)
		}

		return true
	}

	if !isNonTableBlock(tContent) {
		if cells, ok := splitTableRow(rowPart); ok {
			p.body = append(p.body, cells)

			if suffix != "" {
				flushTable()
				emitLine(suffix)
			}

			return true
		}
	}

	flushTable()

	return false
}

// extractTableLine splits a candidate table row into its leading prefix (blockquote or indent)
// and table content, without misinterpreting leading table pipes when BlockquoteChar is "|".
func extractTableLine(line string, opts ProcessMessageOpts) (string, string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return "", "", false
	}

	isQuote := strings.HasPrefix(trimmed, BlockQuoteCharDefault) ||
		(opts.BlockquoteChar != "" && opts.BlockquoteChar != BlockQuoteCharDefault &&
			strings.HasPrefix(trimmed, opts.BlockquoteChar) &&
			(opts.BlockquoteChar != "|" || strings.HasPrefix(strings.TrimLeft(trimmed[1:], " \t"), "|")))

	if isQuote {
		if qp, qrest, ok := splitQuotePrefix(line, opts.BlockquoteChar, opts.InlineCodeChar); ok {
			return qp, strings.TrimSpace(qrest), true
		}
	}

	return line[:len(line)-len(trimmed)], strings.TrimSpace(trimmed), false
}

func stripTableTrailingSuffix(line string) (string, string) {
	lastPipe := strings.LastIndexByte(line, '|')
	if lastPipe >= 0 {
		if after := strings.TrimSpace(line[lastPipe+1:]); strings.HasPrefix(after, "\x1d(") && strings.HasSuffix(after, ")\x1d") {
			return line[:lastPipe+1], after
		}
	}

	return line, ""
}

// splitTableRow splits a GFM pipe table row into trimmed cells, respecting
// escaped pipes (\|) and inline code spans (`...`).
//
//nolint:funlen,gocyclo
func splitTableRow(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.ContainsRune(trimmed, '|') {
		return nil, false
	}

	var (
		cells     []string
		cell      strings.Builder
		pipeCount int
	)

	endedOnPipe := false

	for i := 0; i < len(trimmed); {
		endedOnPipe = false

		switch trimmed[i] {
		case '`':
			_, nextIdx, ok := parseCodeSpan(trimmed, i)

			span := trimmed[i:nextIdx]
			if ok {
				span = strings.ReplaceAll(span, `\|`, "|")
			}

			cell.WriteString(span)

			i = nextIdx
		case '\\':
			if i+1 < len(trimmed) && (trimmed[i+1] == '|' || trimmed[i+1] == '\\') {
				cell.WriteString(trimmed[i : i+2])

				i += 2

				continue
			}

			cell.WriteByte(trimmed[i])

			i++
		case '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()

			pipeCount++
			endedOnPipe = true
			i++
		default:
			cell.WriteByte(trimmed[i])

			i++
		}
	}

	if pipeCount == 0 {
		return nil, false
	}

	cells = append(cells, strings.TrimSpace(cell.String()))

	if trimmed[0] == '|' && cells[0] == "" {
		cells = cells[1:]
	}

	if endedOnPipe && len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}

	return cells, len(cells) > 0
}

func parseTableDelimiterRow(line string, expectedCols int) ([]tableAlign, bool) {
	cells, ok := splitTableRow(line)
	if !ok || len(cells) != expectedCols {
		return nil, false
	}

	aligns := make([]tableAlign, len(cells))

	for i, c := range cells {
		leftColon := strings.HasPrefix(c, ":")
		c = strings.TrimPrefix(c, ":")
		rightColon := strings.HasSuffix(c, ":")
		c = strings.TrimSuffix(c, ":")

		if c == "" || strings.Trim(c, "-") != "" {
			return nil, false
		}

		switch {
		case leftColon && rightColon:
			aligns[i] = tableAlignCenter
		case rightColon:
			aligns[i] = tableAlignRight
		}
	}

	return aligns, true
}

func ircDisplayWidth(s string) int {
	if strings.ContainsRune(s, '\x03') {
		s = stripIrcColors(s)
	}

	if strings.ContainsRune(s, '\x04') {
		s = stripHexColors(s)
	}

	return StringWidth(s)
}

func formatTableRowCells(row []string, numCols int, colWidths []int, opts ProcessMessageOpts, bold bool) ([]string, []int) {
	fmtCells := make([]string, numCols)
	widths := make([]int, numCols)

	for c := range min(len(row), numCols) {
		if row[c] == "" {
			continue
		}

		cell := formatInlineMarkdown(row[c], opts.InlineCodeChar, bold)
		if !opts.DisableEmoji {
			cell = EmojiReplaceAliases(cell, opts.CustomEmoji)
		}

		fmtCells[c] = cell
		widths[c] = ircDisplayWidth(cell)
		colWidths[c] = max(colWidths[c], widths[c])
	}

	return fmtCells, widths
}

func buildTableBorder(prefix string, colWidths []int, left, mid, right string) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(left)

	for c, w := range colWidths {
		if c > 0 {
			b.WriteString(mid)
		}

		b.WriteString(strings.Repeat("─", w+2))
	}

	b.WriteString(right)

	return b.String()
}

func buildTableRow(prefix string, cells []string, widths, colWidths []int, aligns []tableAlign) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("│")

	for c, cell := range cells {
		pad := colWidths[c] - widths[c]
		leftPad, rightPad := 0, pad

		switch aligns[c] {
		case tableAlignRight:
			leftPad, rightPad = pad, 0
		case tableAlignCenter:
			leftPad = pad / 2
			rightPad = pad - leftPad
		}

		b.WriteByte(' ')
		b.WriteString(strings.Repeat(" ", leftPad))
		b.WriteString(cell)
		b.WriteString(strings.Repeat(" ", rightPad))
		b.WriteString(" │")
	}

	return b.String()
}

// formatTable renders a GFM table using Unicode box-drawing characters and
// visual-width-aware column padding/alignment.
func formatTable(header []string, aligns []tableAlign, body [][]string, prefix string, opts ProcessMessageOpts, yield func(string)) {
	numCols := len(header)
	for _, row := range body {
		numCols = max(numCols, len(row))
	}

	for len(aligns) < numCols {
		aligns = append(aligns, tableAlignLeft)
	}

	colWidths := make([]int, numCols)
	for i := range colWidths {
		colWidths[i] = 1
	}

	fmtHeader, hdrWidths := formatTableRowCells(header, numCols, colWidths, opts, true)

	fmtBody := make([][]string, len(body))
	bodyWidths := make([][]int, len(body))

	for r, row := range body {
		fmtBody[r], bodyWidths[r] = formatTableRowCells(row, numCols, colWidths, opts, false)
	}

	yield(buildTableBorder(prefix, colWidths, "┌", "┬", "┐"))
	yield(buildTableRow(prefix, fmtHeader, hdrWidths, colWidths, aligns))

	if len(fmtBody) > 0 {
		yield(buildTableBorder(prefix, colWidths, "├", "┼", "┤"))

		for r := range fmtBody {
			yield(buildTableRow(prefix, fmtBody[r], bodyWidths[r], colWidths, aligns))
		}
	}

	yield(buildTableBorder(prefix, colWidths, "└", "┴", "┘"))
}

// WrapMessage soft-wraps msg into lines of at most maxLen bytes, breaking
// only at spaces/newlines. Words longer than maxLen are left unbroken and
// overflow their line rather than being split unless hardLineSplit is enabled.
//
//nolint:funlen,gocyclo,gocognit
func WrapMessage(msg string, maxLen int, hardLineSplit ...bool) string {
	if maxLen <= 0 || len(msg) <= maxLen {
		return msg
	}

	enableHardLineSplit := len(hardLineSplit) > 0 && hardLineSplit[0]

	var (
		b           strings.Builder
		initialized bool
		lastWrite   int
	)

	linePrefix := ""
	prefixEnd := 0
	lineStart := 0
	lineLen := 0
	wordsOnLine := 0
	spaceStart, spaceLen := 0, 0

	i := 0
	for i < len(msg) {
		switch msg[i] {
		case ' ', '\t':
			spaceStart = i
			for i < len(msg) && (msg[i] == ' ' || msg[i] == '\t') {
				i++
			}

			spaceLen = i - spaceStart

		case '\n':
			if initialized {
				b.WriteString(msg[lastWrite : i+1])
				lastWrite = i + 1
			}

			lineLen, spaceLen = 0, 0
			wordsOnLine = 0
			linePrefix = ""
			prefixEnd = 0
			lineStart = i + 1
			i++

		default:
			if lineLen == 0 {
				linePrefix, prefixEnd = getWrapPrefix(msg[i:])
				prefixEnd += i

				if enableHardLineSplit && len(linePrefix) >= maxLen {
					linePrefix = ""
				}
			}

			start := i
			for i < len(msg) && msg[i] != ' ' && msg[i] != '\t' && msg[i] != '\n' {
				i++
			}

			wordLen := i - start
			isContentWord := start >= prefixEnd

			// If this word pushes us over the limit, inject a newline (only if content is already on the line)
			if wordsOnLine > 0 && lineLen+spaceLen+wordLen > maxLen { //nolint:nestif
				if !initialized {
					b.Grow(len(msg) + (len(msg)/maxLen)*2)

					initialized = true
				}

				// Write everything from our last position up to the spaces we are splitting on
				b.WriteString(msg[lastWrite:spaceStart])
				b.WriteByte('\n') // Inject the wrap

				if linePrefix != "" {
					b.WriteString(linePrefix)
				}

				// Skip over the spaces we just wrapped on
				lastWrite = spaceStart + spaceLen

				lineLen = wordLen + len(linePrefix)
				spaceLen = 0

				if isContentWord {
					wordsOnLine = 1
				} else {
					wordsOnLine = 0
				}
			} else {
				lineLen += spaceLen + wordLen
				spaceLen = 0

				if isContentWord {
					wordsOnLine++
				}
			}

			if enableHardLineSplit && lineLen > maxLen {
				if !initialized {
					b.Grow(len(msg) + (len(msg)/maxLen)*2)
					b.WriteString(msg[lastWrite:lineStart])
					lastWrite = lineStart
					initialized = true
				}

				for lineLen > maxLen {
					chunkLimit := max(1, maxLen-len(linePrefix))
					splitPos := lastWrite + chunkLimit

					if splitPos >= i {
						break
					}

					for splitPos > lastWrite && (msg[splitPos]&0xc0) == 0x80 {
						splitPos--
					}

					// Ensure at least one full rune advances if budget is smaller than rune size
					if splitPos == lastWrite {
						splitPos = lastWrite + 1

						for splitPos < i && (msg[splitPos]&0xc0) == 0x80 {
							splitPos++
						}
					}

					b.WriteString(msg[lastWrite:splitPos])
					b.WriteByte('\n')

					if linePrefix != "" {
						b.WriteString(linePrefix)
					}

					lastWrite = splitPos
					lineLen = (i - splitPos) + len(linePrefix)
					wordsOnLine = 1
				}
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

//nolint:funlen,gocyclo,gocognit
func getWrapPrefix(s string) (string, int) {
	idx := 0

	// Skip bracketed prefix tags if present (e.g. timestamp "[2006-01-02 15:04]" or thread tag "[↪@@...]")
	for idx < len(s) && s[idx] == '[' {
		closeIdx := strings.IndexByte(s[idx:], ']')
		if closeIdx < 0 || closeIdx > 80 {
			break
		}

		idx += closeIdx + 1

		for idx < len(s) && (s[idx] == ' ' || s[idx] == '\t') {
			idx++
		}
	}

	rem := s[idx:]
	limit := min(len(rem), 24)
	sub := rem[:limit]

	i := 0
	for i < len(sub) {
		switch sub[i] {
		case '\x02':
			i++
		case '\x03':
			i++

			for i < len(sub) && sub[i] >= '0' && sub[i] <= '9' {
				i++
			}

			if i < len(sub) && sub[i] == ',' {
				i++

				for i < len(sub) && sub[i] >= '0' && sub[i] <= '9' {
					i++
				}
			}
		case '\x04':
			i += 7
		default:
			goto checkBar
		}
	}

checkBar:
	if i >= len(sub) {
		return "", idx
	}

	var barLen int

	switch {
	case sub[i] == '|':
		barLen = 1
	case strings.HasPrefix(sub[i:], "🮇"):
		barLen = len("🮇")
	case strings.HasPrefix(sub[i:], "│"):
		barLen = len("│")
	case strings.HasPrefix(sub[i:], "┃"):
		barLen = len("┃")
	case strings.HasPrefix(sub[i:], "▕"):
		barLen = len("▕")
	default:
		return "", idx
	}

	end := idx + i + barLen

	for end < len(s) {
		switch {
		case s[end] == '\x0f' || s[end] == ' ' || s[end] == '\t':
			end++
		case end+1 < len(s) && s[end] == 0xc2 && s[end+1] == 0xa0:
			end += 2
		default:
			return s[idx:end], end
		}
	}

	return s[idx:end], end
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

	default: // "all"
		for range count {
			yield("")
		}
	}
}

func matchDefaultMention(text string, start int) int {
	switch {
	case strings.HasPrefix(text[start:], "channel") && checkWordBoundaryEnd(text, start+7):
		return 7
	case strings.HasPrefix(text[start:], "everyone") && checkWordBoundaryEnd(text, start+8):
		return 8
	case strings.HasPrefix(text[start:], "here") && checkWordBoundaryEnd(text, start+4):
		return 4
	case strings.HasPrefix(text[start:], "all") && checkWordBoundaryEnd(text, start+3):
		return 3
	case strings.HasPrefix(text[start:], "room") && checkWordBoundaryEnd(text, start+4):
		return 4
	default:
		return 0
	}
}

// SanitizeDefaultMentions defangs @channel, @all, @here, @everyone, and Matrix @room mentions to prevent
// unwanted highlights in IRC clients. When useUnicode is enabled, it inserts a
// Zero-Width Non-Joiner (ZWNJ) after the '@'. When disabled, it inserts a hyphen.
func SanitizeDefaultMentions(text string, useUnicode bool) string {
	if !strings.Contains(text, "@channel") && !strings.Contains(text, "@all") && !strings.Contains(text, "@here") && !strings.Contains(text, "@room") && !strings.Contains(text, "@everyone") {
		return text
	}

	separator := "@-"
	if useUnicode {
		separator = "@\u200c"
	}

	var (
		sb   strings.Builder
		last int
	)

	for i := 0; i < len(text); {
		if text[i] != '@' {
			i++

			continue
		}

		matchedLen := matchDefaultMention(text, i+1)
		if matchedLen == 0 {
			i++

			continue
		}

		if last == 0 {
			sb.Grow(len(text) + 16)
		}

		sb.WriteString(text[last:i])
		sb.WriteString(separator)
		sb.WriteString(text[i+1 : i+1+matchedLen])

		i += 1 + matchedLen
		last = i
	}

	if last == 0 {
		return text
	}

	sb.WriteString(text[last:])

	return sb.String()
}

// StringWidth returns the visual cell width of a string in a monospace terminal,
// accounting for East Asian wide characters, combining marks, format characters,
// regional indicator pairs (flags), and emoji sequences.
//
//nolint:funlen
func StringWidth(s string) int {
	width := 0
	inZWJ := false
	inRI := false
	prevWasEmoji := false
	prevEmojiNarrow := false
	prevWideFallback := false

	for _, r := range s {
		if r == '\u200d' {
			if prevWasEmoji {
				inZWJ = true
			}

			prevWideFallback = false

			continue
		}

		if isZeroWidth(r) {
			added, narrow := checkEmojiPresentationUpgrade(r, prevEmojiNarrow)
			sub, fallback := checkTextPresentation(r, prevWideFallback)

			width += added - sub
			prevEmojiNarrow = narrow
			prevWideFallback = fallback

			if added > 0 {
				prevWasEmoji = true
			} else if r == '\ufe0e' {
				prevWasEmoji = false
				prevEmojiNarrow = false
			}

			continue
		}

		if inZWJ {
			inZWJ = false

			if isEmoji(r) {
				prevWasEmoji = true
				prevEmojiNarrow = false
				prevWideFallback = false

				continue
			}
		}

		if isRegionalIndicator(r) {
			delta, nextRI := stepRegionalIndicator(inRI)

			inRI = nextRI
			width += delta
			prevWasEmoji = true
			prevEmojiNarrow = false
			prevWideFallback = false

			continue
		}

		inRI = false

		if isWide(r) {
			width += 2
			prevWasEmoji = isEmoji(r)
			prevEmojiNarrow = false
			prevWideFallback = isWideFallback(r)

			continue
		}

		width++
		prevWasEmoji = isEmoji(r)
		prevEmojiNarrow = prevWasEmoji
		prevWideFallback = false
	}

	return width
}

func checkEmojiPresentationUpgrade(r rune, prevEmojiNarrow bool) (int, bool) {
	if (r == '\ufe0f' || r == 0x20e3 || isFitzpatrickModifier(r)) && prevEmojiNarrow {
		return 1, false
	}

	return 0, prevEmojiNarrow
}

func checkTextPresentation(r rune, prevWideFallback bool) (int, bool) {
	if r == '\ufe0e' && prevWideFallback {
		return 1, false
	}

	if r == '\ufe0f' {
		return 0, false
	}

	return 0, prevWideFallback
}

func isFitzpatrickModifier(r rune) bool {
	return r >= 0x1f3fb && r <= 0x1f3ff
}

func isRegionalIndicator(r rune) bool {
	return r >= 0x1f1e6 && r <= 0x1f1ff
}

//nolint:gocyclo
func isZeroWidth(r rune) bool {
	if r < 0x20 || (r >= 0x7f && r < 0xa0) || r == 0xad {
		return true
	}

	// Combining characters (Mn, Me) and invisible format characters (Cf, e.g. \u200B and tag characters)
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return true
	}

	// Medial and trailing Hangul Jamo (Jungseong U+1160..U+11A7 and Jongseong U+11A8..U+11FF)
	// and Hangul Jamo Extended-B (U+D7B0..U+D7C6, U+D7CB..U+D7FB)
	if (r >= 0x1160 && r <= 0x11ff) || (r >= 0xd7b0 && r <= 0xd7c6) || (r >= 0xd7cb && r <= 0xd7fb) {
		return true
	}

	// Emoji skin tone modifiers (Fitzpatrick types 1-6)
	if isFitzpatrickModifier(r) {
		return true
	}

	// Variation selectors (VS1-VS16 and VS17-VS256)
	if (r >= 0xfe00 && r <= 0xfe0f) || (r >= 0xe0100 && r <= 0xe01ef) {
		return true
	}

	return false
}

func isWide(r rune) bool {
	return isCJK(r) || isEmojiWide(r)
}

func isWideFallback(r rune) bool {
	return r == 0x203c || r == 0x2049
}

//nolint:gocyclo
func isCJK(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f:
		return true
	case r >= 0x2e80 && r <= 0xa4cf && r != 0x303f:
		return true
	case r >= 0xac00 && r <= 0xd7a3:
		return true
	case r >= 0xf900 && r <= 0xfaff:
		return true
	case r >= 0xfe10 && r <= 0xfe19:
		return true
	case r >= 0xfe30 && r <= 0xfe6f:
		return true
	case r >= 0xff01 && r <= 0xff60:
		return true
	case r >= 0xffe0 && r <= 0xffe6:
		return true
	case r >= 0x1b000 && r <= 0x1b2ff: // Supplementary Kana, Small Kana Extension, Nushu (e.g. U+1B001)
		return true
	case r >= 0x1f200 && r <= 0x1f2ff: // Enclosed Ideographic Supplement
		return true
	case r >= 0x20000 && r <= 0x3fffd: // Plane 2 & Plane 3 CJK Extensions (e.g. 𠀀 U+20000)
		return true
	default:
		return false
	}
}

//nolint:gocyclo
func isEmoji(r rune) bool {
	switch {
	case (r >= '0' && r <= '9') || r == '#' || r == '*':
		return true
	case r == 0xa9 || r == 0xae || r == 0x203c || r == 0x2049 || r == 0x2122 || r == 0x2139:
		return true
	case r >= 0x2194 && r <= 0x21aa:
		return true
	case r >= 0x2300 && r <= 0x27bf:
		return true
	case r >= 0x2934 && r <= 0x2b55:
		return true
	case r == 0x3030 || r == 0x303d || r == 0x3297 || r == 0x3299:
		return true
	case r >= 0x1f000 && r <= 0x1faff:
		return true
	default:
		return false
	}
}

//nolint:funlen,gocyclo
func isEmojiWide(r rune) bool {
	// Standalone wide symbols in SMP
	if r == 0x1f004 || r == 0x1f0cf || r == 0x1f18e || (r >= 0x1f191 && r <= 0x1f19a) {
		return true
	}

	// SMP emoji block (U+1F300..U+1FAFF) excluding text-default symbols
	if r >= 0x1f300 && r <= 0x1faff {
		return !isSMPTextDefault(r)
	}

	// BMP symbols with Emoji_Presentation = Yes (or wide terminal emoji fallback)
	switch {
	case isWideFallback(r):
		return true
	case r == 0x231a || r == 0x231b:
		return true
	case r >= 0x23e9 && r <= 0x23ec:
		return true
	case r == 0x23f0 || r == 0x23f3:
		return true
	case r == 0x25fd || r == 0x25fe:
		return true
	case r == 0x2614 || r == 0x2615:
		return true
	case r >= 0x2648 && r <= 0x2653:
		return true
	case r == 0x267f || r == 0x2693 || r == 0x26a1:
		return true
	case r == 0x26aa || r == 0x26ab || r == 0x26bd || r == 0x26be:
		return true
	case r == 0x26c4 || r == 0x26c5 || r == 0x26ce || r == 0x26d4:
		return true
	case r == 0x26ea || r == 0x26f2 || r == 0x26f3 || r == 0x26f5:
		return true
	case r == 0x26fa || r == 0x26fd:
		return true
	case r == 0x2705: // ✅ U+2705
		return true
	case r == 0x270a || r == 0x270b:
		return true
	case r == 0x2728:
		return true
	case r == 0x274c || r == 0x274e:
		return true
	case (r >= 0x2753 && r <= 0x2755) || r == 0x2757:
		return true
	case r >= 0x2795 && r <= 0x2797:
		return true
	case r == 0x27b0 || r == 0x27bf:
		return true
	case r == 0x2b1b || r == 0x2b1c || r == 0x2b50 || r == 0x2b55:
		return true
	default:
		return false
	}
}

// isSMPTextDefault identifies SMP symbols that default to text presentation
// (Emoji_Presentation = No) unless followed by U+FE0F.
//
//nolint:gocyclo
func isSMPTextDefault(r rune) bool {
	switch {
	case r >= 0x1f321 && r <= 0x1f32c: // Weather (e.g. 🌡)
		return true
	case r == 0x1f336 || r == 0x1f37d: // 🌶, 🍽
		return true
	case r >= 0x1f394 && r <= 0x1f39f: // Badges, media controls
		return true
	case r >= 0x1f3cb && r <= 0x1f3ce: // Sports, vehicles
		return true
	case r >= 0x1f3d4 && r <= 0x1f3df: // Outdoor / buildings
		return true
	case r == 0x1f3f3 || r == 0x1f3f5 || r == 0x1f3f7: // 🏳, 🏵, 🏷
		return true
	case r == 0x1f43f || r == 0x1f441 || r == 0x1f4fd: // 🐿, 👁, 📽
		return true
	case r >= 0x1f549 && r <= 0x1f54a: // 🕉, 🕊
		return true
	case (r >= 0x1f56f && r <= 0x1f570) || (r >= 0x1f573 && r <= 0x1f579): // 🕯, 🕰, 🕹, etc.
		return true
	case r == 0x1f587 || (r >= 0x1f58a && r <= 0x1f58d) || r == 0x1f590: // 🖇, 🖊, 🖐
		return true
	case r == 0x1f5a5 || r == 0x1f5a8 || r == 0x1f5b1 || r == 0x1f5b2 || r == 0x1f5bc: // 🖥 U+1F5A5, 🖨, 🖱, 🖼
		return true
	case (r >= 0x1f5c2 && r <= 0x1f5c4) || (r >= 0x1f5d1 && r <= 0x1f5d3): // Office stationery
		return true
	case (r >= 0x1f5dc && r <= 0x1f5de) || r == 0x1f5e1 || r == 0x1f5e3 || r == 0x1f5e8 || r == 0x1f5ef || r == 0x1f5f3 || r == 0x1f5fa:
		return true
	default:
		return false
	}
}

func stepRegionalIndicator(inRI bool) (int, bool) {
	if inRI {
		return 0, false
	}

	return 2, true
}

// ActionSuffixKind identifies a message state suffix appended to IRC messages.
type ActionSuffixKind string

const (
	SuffixEdited    ActionSuffixKind = "edited"
	SuffixPinned    ActionSuffixKind = "pinned"
	SuffixUnpinned  ActionSuffixKind = "unpinned"
	SuffixUpdated   ActionSuffixKind = "updated"
	SuffixDeleted   ActionSuffixKind = "deleted"
	SuffixEncrypted ActionSuffixKind = "encrypted"
)

// FormatActionSuffix returns the italicized IRC suffix for a message state change.
func FormatActionSuffix(kind ActionSuffixKind, useUnicode bool) string {
	switch kind {
	case SuffixEdited:
		if useUnicode {
			return " \x1d(✏️edited)\x1d"
		}

		return " \x1d(edited)\x1d"
	case SuffixPinned:
		if useUnicode {
			return " \x1d(📌pinned)\x1d"
		}

		return " \x1d(pinned)\x1d"
	case SuffixUnpinned:
		if useUnicode {
			return " \x1d(📌unpinned)\x1d"
		}

		return " \x1d(unpinned)\x1d"
	case SuffixUpdated:
		if useUnicode {
			return " \x1d(🔄updated)\x1d"
		}

		return " \x1d(updated)\x1d"
	case SuffixDeleted:
		if useUnicode {
			return " \x1d(🗑️deleted)\x1d"
		}

		return " \x1d(deleted)\x1d"
	case SuffixEncrypted:
		if useUnicode {
			return " \x1d(🔒encrypted)\x1d"
		}

		return " \x1d(encrypted)\x1d"
	default:
		return ""
	}
}

// AppendMessageSuffix appends a formatted suffix (such as a reply context or action suffix)
// to msg, stripping trailing newlines and placing single-line CTCP ACTION suffixes inside \x01.
func AppendMessageSuffix(msg, suffix string) string {
	msg = strings.TrimRight(msg, "\r\n")
	if suffix == "" {
		return msg
	}

	if strings.HasPrefix(msg, "\x01ACTION ") && strings.HasSuffix(msg, "\x01") && !strings.Contains(msg, "\n") {
		return msg[:len(msg)-1] + suffix + "\x01"
	}

	return strings.TrimLeft(msg+suffix, " ")
}

// FormatLastOnline formats a Unix timestamp (seconds) as a human-readable
// "Last online ..." or "Last activity ..." string for WHOIS output.
func FormatLastOnline(lastActivity int64, isOnline ...bool) string {
	if lastActivity <= 0 {
		return ""
	}

	label := "Last online"
	if len(isOnline) > 0 && isOnline[0] {
		label = "Last activity"
	}

	diff := max(0, time.Now().Unix()-lastActivity)

	switch {
	case diff < 45:
		return label + " just now"
	case diff < 3300: // < 55 minutes
		mins := max(1, (diff+30)/60)
		if mins == 1 {
			return label + " 1 min. ago"
		}

		return fmt.Sprintf("%s %d mins. ago", label, mins)
	case diff < 84600: // < 23.5 hours
		hours := (diff + 1800) / 3600
		if hours == 1 {
			return label + " 1 hr. ago"
		}

		return fmt.Sprintf("%s %d hr. ago", label, hours)
	case diff < 30*86400:
		days := (diff + 43200) / 86400
		if days == 1 {
			return label + " 1 day ago"
		}

		return fmt.Sprintf("%s %d days ago", label, days)
	default:
		return fmt.Sprintf("%s %s", label, time.Unix(lastActivity, 0).Format("2006-01-02"))
	}
}

// ExpandDirectoryURL expands {nick}, {username}, {email}, {firstname}, and {lastname}
// placeholders in a DirectoryURL template, escaping them appropriately for the
// path and query components.
func ExpandDirectoryURL(tmpl, nick, username string, extra ...string) string {
	if tmpl == "" {
		return ""
	}

	var email, firstName, lastName string
	if len(extra) > 0 {
		email = extra[0]
	}

	if len(extra) > 1 {
		firstName = extra[1]
	}

	if len(extra) > 2 {
		lastName = extra[2]
	}

	pathPart, queryPart, hasQuery := strings.Cut(tmpl, "?")
	result := expandDirectoryPart(pathPart, nick, username, email, firstName, lastName, neturl.PathEscape)

	if hasQuery {
		result += "?" + expandDirectoryPart(queryPart, nick, username, email, firstName, lastName, neturl.QueryEscape)
	}

	return result
}

func expandDirectoryPart(part, nick, username, email, firstName, lastName string, escape func(string) string) string {
	if strings.Contains(part, "{username}") {
		part = strings.ReplaceAll(part, "{username}", escape(username))
	}

	if strings.Contains(part, "{nick}") {
		part = strings.ReplaceAll(part, "{nick}", escape(nick))
	}

	if strings.Contains(part, "{email}") {
		part = strings.ReplaceAll(part, "{email}", escape(email))
	}

	if strings.Contains(part, "{firstname}") {
		part = strings.ReplaceAll(part, "{firstname}", escape(firstName))
	}

	if strings.Contains(part, "{lastname}") {
		part = strings.ReplaceAll(part, "{lastname}", escape(lastName))
	}

	return part
}
