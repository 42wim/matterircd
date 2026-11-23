package googlechatclient

import (
	"cmp"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

type textSpan struct {
	start      int
	end        int
	open       string
	close      string
	priority   int
	formatType int32
	block      bool
}

type outgoingAnnotation struct {
	Type        uint64
	StartIndex  int32
	Length      int32
	UserID      string
	DisplayName string
	MentionAll  bool
	FormatType  uint64
}

// buildAttachmentURL builds an authenticated Google Chat attachment download URL
// matching purple-googlechat's get_attachment_url convention.
func (c *Client) buildAttachmentURL(token, contentType string) string {
	if token == "" {
		return ""
	}

	urlType := "DOWNLOAD_URL"
	if contentType == "" || strings.HasPrefix(strings.ToLower(contentType), "image/") {
		urlType = "FIFE_URL"
	}

	params := url.Values{}
	params.Set("url_type", urlType)

	if contentType != "" {
		params.Set("content_type", contentType)
	}

	params.Set("attachment_token", token)

	return c.chatBaseURL() + "/api/get_attachment_url?" + params.Encode()
}

// extractMentionsAndAttachments converts Dynamite Message annotations into
// normalized Mentions, Attachments, and any appended meet/link info.
func (c *Client) extractMentionsAndAttachments(pm *protoMessage) ([]Mention, []Attachment, string) {
	if pm == nil {
		return nil, nil, ""
	}

	if len(pm.Annotations) == 0 {
		return nil, nil, pm.TextBody
	}

	var (
		mentions    []Mention
		attachments []Attachment
	)

	text := c.formatAnnotatedText(pm.TextBody, pm.Annotations)

	for _, ann := range pm.Annotations {
		c.applySingleAnnotation(ann, &mentions, &attachments, &text)
	}

	return mentions, attachments, text
}

func (c *Client) formatAnnotatedText(text string, anns []protoAnnotation) string {
	if text == "" || len(anns) == 0 {
		return text
	}

	spans, hidden := c.collectFormatSpans(text, anns)
	if len(spans) == 0 && len(hidden) == 0 {
		return text
	}

	resolveItalicOverlaps(spans)

	return renderFormattedText(text, spans, hidden)
}

func (c *Client) collectFormatSpans(text string, anns []protoAnnotation) ([]textSpan, [][2]int) {
	totalUTF16 := utf16Len(text)

	var hidden [][2]int

	for _, ann := range anns {
		if ann.Type != protoAnnotationFormatData || ann.Length <= 0 {
			continue
		}

		if ann.FormatType != protoFormatHidden && ann.FormatType != protoFormatClientHidden {
			continue
		}

		start := max(0, int(ann.StartIndex))
		end := min(totalUTF16, start+int(ann.Length))

		if start < end {
			hidden = append(hidden, [2]int{start, end})
		}
	}

	var spans []textSpan

	for _, ann := range anns {
		if ann.Length <= 0 {
			continue
		}

		start := max(0, int(ann.StartIndex))
		end := min(totalUTF16, start+int(ann.Length))

		if start >= end {
			continue
		}

		spans = c.appendAnnotationSpan(spans, text, ann, start, end, hidden)
	}

	return spans, hidden
}

func (c *Client) appendAnnotationSpan(
	spans []textSpan,
	text string,
	ann protoAnnotation,
	start, end int,
	hidden [][2]int,
) []textSpan {
	switch ann.Type {
	case protoAnnotationFormatData:
		if ann.FormatType == protoFormatHidden || ann.FormatType == protoFormatClientHidden {
			return spans
		}

		open, closeDelim, priority, block, ok := formatTypeDelimiters(ann.FormatType)
		if !ok {
			if !isNoopFormatType(ann.FormatType) {
				c.logger.Debugf("Unrecognized FormatMetadata format_type=%d on span [%d:%d]", ann.FormatType, start, end)
			}

			return spans
		}

		sp := textSpan{
			start:      start,
			end:        end,
			open:       open,
			close:      closeDelim,
			priority:   priority,
			formatType: ann.FormatType,
			block:      block,
		}

		return appendNormalizedSpan(spans, text, sp, hidden)

	case protoAnnotationURL:
		if ann.URL == "" {
			return spans
		}

		vis := strings.TrimSpace(visibleTextInRange(text, start, end, hidden))
		if vis == "" || isPlainURLText(vis, ann.URL) {
			return spans
		}

		sp := textSpan{
			start:    start,
			end:      end,
			open:     "[",
			close:    "](" + ann.URL + ")",
			priority: 15,
		}

		return appendNormalizedSpan(spans, text, sp, hidden)

	default:
		if !isKnownNonFormatAnnotation(ann) {
			c.logger.Debugf("Unrecognized text annotation type=%d format_type=%d on span [%d:%d]", ann.Type, ann.FormatType, start, end)
		}

		return spans
	}
}

func formatTypeDelimiters(ft int32) (string, string, int, bool, bool) {
	switch ft {
	case protoFormatBold:
		return "**", "**", 20, false, true
	case protoFormatItalic:
		return "*", "*", 30, false, true
	case protoFormatStrike:
		return "~~", "~~", 40, false, true
	case protoFormatUnderline:
		return "\x1f", "\x1f", 50, false, true
	case protoFormatSourceCode, protoFormatMonospace:
		return "`", "`", 60, false, true
	case protoFormatMonospaceBlock:
		return "```\n", "\n```", 10, true, true
	case protoFormatBulletedListItem:
		return "- ", "", 5, true, true
	case protoFormatBlockquote:
		return "> ", "", 5, true, true
	default:
		return "", "", 0, false, false
	}
}

func isNoopFormatType(ft int32) bool {
	switch ft {
	case 0, protoFormatFontColor, protoFormatBulletedList, protoFormatSearchHighlight:
		return true
	default:
		return false
	}
}

func isKnownNonFormatAnnotation(ann protoAnnotation) bool {
	if ann.Type == protoAnnotationUserMention || ann.MentionUserID != "" || ann.MentionType != 0 {
		return true
	}

	if ann.UploadToken != "" || ann.DriveID != "" || ann.MeetURL != "" || ann.CustomEmoji != "" {
		return true
	}

	return ann.RoomNewName != "" || ann.RoomNewDesc != "" || ann.MemberAction != 0
}

func isPlainURLText(vis, rawURL string) bool {
	if strings.EqualFold(vis, rawURL) {
		return true
	}

	trimmed := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")

	return strings.EqualFold(vis, trimmed) || strings.EqualFold(strings.TrimSuffix(vis, "/"), strings.TrimSuffix(trimmed, "/"))
}

func appendNormalizedSpan(spans []textSpan, text string, sp textSpan, hidden [][2]int) []textSpan {
	if sp.formatType == protoFormatBlockquote {
		return appendBlockquoteLines(spans, text, sp, hidden)
	}

	if sp.block {
		if visibleTextInRange(text, sp.start, sp.end, hidden) == "" {
			return spans
		}

		return append(spans, sp)
	}

	// Split inline spans at newlines so line-oriented IRC markdown formatters handle each line.
	segStart := sp.start
	pos := 0

	for _, r := range text {
		w := utf16Width(r)

		if r == '\n' && pos >= segStart && pos < sp.end {
			if trimmed, ok := trimInlineSpan(text, sp, segStart, pos, hidden); ok {
				spans = append(spans, trimmed)
			}

			segStart = pos + w
		}

		pos += w
		if pos >= sp.end {
			break
		}
	}

	if segStart < sp.end {
		if trimmed, ok := trimInlineSpan(text, sp, segStart, sp.end, hidden); ok {
			spans = append(spans, trimmed)
		}
	}

	return spans
}

func appendBlockquoteLines(spans []textSpan, text string, sp textSpan, hidden [][2]int) []textSpan {
	segStart := sp.start
	pos := 0

	for _, r := range text {
		w := utf16Width(r)

		if r == '\n' && pos >= segStart && pos < sp.end {
			if visibleTextInRange(text, segStart, pos, hidden) != "" {
				lineSpan := sp
				lineSpan.start = segStart
				lineSpan.end = pos
				spans = append(spans, lineSpan)
			}

			segStart = pos + w
		}

		pos += w
		if pos >= sp.end {
			break
		}
	}

	if segStart < sp.end && visibleTextInRange(text, segStart, sp.end, hidden) != "" {
		lineSpan := sp
		lineSpan.start = segStart
		lineSpan.end = sp.end
		spans = append(spans, lineSpan)
	}

	return spans
}

func trimInlineSpan(text string, base textSpan, start, end int, hidden [][2]int) (textSpan, bool) {
	firstVis := -1
	lastVisEnd := -1
	pos := 0

	for _, r := range text {
		w := utf16Width(r)

		if pos >= end {
			break
		}

		if pos >= start && !isPosHidden(pos, hidden) && r != ' ' && r != '\t' && r != '\r' {
			if firstVis < 0 {
				firstVis = pos
			}

			lastVisEnd = pos + w
		}

		pos += w
	}

	if firstVis < 0 || lastVisEnd <= firstVis {
		return textSpan{}, false
	}

	out := base
	out.start = firstVis
	out.end = lastVisEnd

	return out, true
}

func resolveItalicOverlaps(spans []textSpan) {
	for i := range spans {
		if spans[i].formatType != protoFormatItalic {
			continue
		}

		for j := range spans {
			if spans[j].formatType != protoFormatBold {
				continue
			}

			overlaps := spans[j].start < spans[i].end && spans[i].start < spans[j].end
			sameBounds := spans[j].start == spans[i].start && spans[j].end == spans[i].end

			if overlaps && !sameBounds {
				spans[i].open = "_"
				spans[i].close = "_"

				break
			}
		}
	}
}

func renderFormattedText(text string, spans []textSpan, hidden [][2]int) string {
	opens := slices.Clone(spans)
	slices.SortFunc(opens, func(a, b textSpan) int {
		if c := cmp.Compare(a.start, b.start); c != 0 {
			return c
		}

		if c := cmp.Compare(b.end, a.end); c != 0 {
			return c
		}

		return cmp.Compare(a.priority, b.priority)
	})

	closes := slices.Clone(spans)
	slices.SortFunc(closes, func(a, b textSpan) int {
		if c := cmp.Compare(a.end, b.end); c != 0 {
			return c
		}

		if c := cmp.Compare(b.start, a.start); c != 0 {
			return c
		}

		return cmp.Compare(b.priority, a.priority)
	})

	var b strings.Builder

	b.Grow(len(text) + len(spans)*4)

	oi, ci, pos := 0, 0, 0

	for _, r := range text {
		for ci < len(closes) && closes[ci].end <= pos {
			writeSpanClose(&b, closes[ci])
			ci++
		}

		for oi < len(opens) && opens[oi].start <= pos {
			writeSpanOpen(&b, opens[oi])
			oi++
		}

		if !isPosHidden(pos, hidden) {
			b.WriteRune(r)
		}

		pos += utf16Width(r)
	}

	for ci < len(closes) {
		writeSpanClose(&b, closes[ci])
		ci++
	}

	return b.String()
}

func writeSpanOpen(b *strings.Builder, sp textSpan) {
	if sp.open == "" {
		return
	}

	if sp.block && b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte('\n')
	}

	b.WriteString(sp.open)
}

func writeSpanClose(b *strings.Builder, sp textSpan) {
	if sp.close == "" {
		return
	}

	if sp.close == "\n```" && strings.HasSuffix(b.String(), "\n") {
		b.WriteString("```")

		return
	}

	b.WriteString(sp.close)
}

func utf16Width(r rune) int {
	if r >= 0x10000 {
		return 2
	}

	return 1
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16Width(r)
	}

	return n
}

func isPosHidden(pos int, hidden [][2]int) bool {
	for _, h := range hidden {
		if pos >= h[0] && pos < h[1] {
			return true
		}
	}

	return false
}

func visibleTextInRange(text string, start, end int, hidden [][2]int) string {
	var b strings.Builder

	pos := 0

	for _, r := range text {
		w := utf16Width(r)

		if pos >= end {
			break
		}

		if pos >= start && !isPosHidden(pos, hidden) {
			b.WriteRune(r)
		}

		pos += w
	}

	return b.String()
}

func (c *Client) applySingleAnnotation(ann protoAnnotation, mentions *[]Mention, attachments *[]Attachment, text *string) {
	if ann.Type == protoAnnotationUserMention || ann.MentionUserID != "" || ann.MentionType == protoMentionAll {
		if ann.MentionType == protoMentionAll {
			*mentions = append(*mentions, Mention{All: true, DisplayName: "all"})
		} else if ann.MentionUserID != "" {
			*mentions = append(*mentions, Mention{
				UserID:      ann.MentionUserID,
				DisplayName: ann.MentionName,
			})
		}

		return
	}

	if ann.UploadToken != "" {
		name := ann.UploadName
		if name == "" {
			name = "attachment"
		}

		*attachments = append(*attachments, Attachment{
			ID:          ann.UploadToken,
			Name:        name,
			ContentType: ann.UploadMIME,
			URL:         c.buildAttachmentURL(ann.UploadToken, ann.UploadMIME),
		})

		return
	}

	if ann.DriveID != "" {
		driveURL := ann.DriveURL
		if driveURL == "" {
			driveURL = "https://drive.google.com/open?id=" + url.QueryEscape(ann.DriveID)
		}

		name := ann.DriveTitle
		if name == "" {
			name = ann.DriveID
		}

		*attachments = append(*attachments, Attachment{
			ID:          ann.DriveID,
			Name:        name,
			ContentType: ann.DriveMIME,
			URL:         driveURL,
		})

		return
	}

	if ann.MeetURL != "" && !strings.Contains(*text, ann.MeetURL) {
		if *text != "" {
			*text += " "
		}

		*text += ann.MeetURL
	}
}

type outgoingBuilder struct {
	out        strings.Builder
	utf16Pos   int
	anns       []outgoingAnnotation
	resolveRef func(token string) (string, string)
}

func (b *outgoingBuilder) writeString(s string) {
	b.out.WriteString(s)
	b.utf16Pos += utf16Len(s)
}

func (b *outgoingBuilder) addFormat(start, end int, ft uint64) {
	if end <= start {
		return
	}

	b.anns = append(b.anns, outgoingAnnotation{
		Type:       protoAnnotationFormatData,
		StartIndex: int32(start),       //nolint:gosec // bounded message length <= 4096
		Length:     int32(end - start), //nolint:gosec // bounded message length <= 4096
		FormatType: ft,
	})
}

func (b *outgoingBuilder) addMention(start, end int, userID, displayName string, all bool) {
	if end <= start {
		return
	}

	b.anns = append(b.anns, outgoingAnnotation{
		Type:        protoAnnotationUserMention,
		StartIndex:  int32(start),       //nolint:gosec // bounded message length <= 4096
		Length:      int32(end - start), //nolint:gosec // bounded message length <= 4096
		UserID:      userID,
		DisplayName: displayName,
		MentionAll:  all,
	})
}

// buildOutgoingAnnotations parses outgoing Markdown/IRC formatting and @mentions
// into plain text plus Dynamite FormatMetadata / UserMentionMetadata annotations
// using UTF-16 code unit offsets.
func (c *Client) buildOutgoingAnnotations(text string) (string, []outgoingAnnotation) {
	if text == "" {
		return "", nil
	}

	// Fast path when there are no formatting markers or mention prefixes.
	if !strings.ContainsAny(text, "*_~`\x1f>@") {
		return text, nil
	}

	var resolveRef func(string) (string, string)
	if c != nil {
		resolveRef = c.ResolveMention
	}

	b := &outgoingBuilder{resolveRef: resolveRef}
	b.out.Grow(len(text))
	b.parseBlocks(text)

	if len(b.anns) == 0 {
		return b.out.String(), nil
	}

	return b.out.String(), b.anns
}

func (b *outgoingBuilder) parseBlocks(text string) {
	lines := strings.Split(text, "\n")

	for idx := 0; idx < len(lines); idx++ {
		if idx > 0 {
			b.writeString("\n")
		}

		if nextIdx, ok := b.consumeFencedBlock(lines, idx); ok {
			idx = nextIdx

			continue
		}

		line := lines[idx]
		if after, ok := strings.CutPrefix(line, "> "); ok {
			start := b.utf16Pos
			b.parseInline(after)
			b.addFormat(start, b.utf16Pos, protoFormatBlockquote)

			continue
		}

		b.parseInline(line)
	}
}

func (b *outgoingBuilder) consumeFencedBlock(lines []string, idx int) (int, bool) {
	if !strings.HasPrefix(strings.TrimSpace(lines[idx]), "```") {
		return idx, false
	}

	closeIdx := -1

	for j := idx + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "```" {
			closeIdx = j

			break
		}
	}

	if closeIdx < 0 {
		return idx, false
	}

	content := strings.Join(lines[idx+1:closeIdx], "\n")
	start := b.utf16Pos
	b.writeString(content)
	b.addFormat(start, b.utf16Pos, protoFormatMonospaceBlock)

	return closeIdx, true
}

func (b *outgoingBuilder) parseInline(line string) {
	for i := 0; i < len(line); {
		if next, ok := b.tryInlineToken(line, i); ok {
			i = next

			continue
		}

		r, size := utf8.DecodeRuneInString(line[i:])
		b.out.WriteString(line[i : i+size])
		b.utf16Pos += utf16Width(r)
		i += size
	}
}

func (b *outgoingBuilder) tryInlineToken(line string, i int) (int, bool) {
	switch line[i] {
	case '`':
		if j := strings.IndexByte(line[i+1:], '`'); j > 0 {
			start := b.utf16Pos
			b.writeString(line[i+1 : i+1+j])
			b.addFormat(start, b.utf16Pos, protoFormatMonospace)

			return i + 1 + j + 1, true
		}

		return i, false

	case '\x1f':
		if j := strings.IndexByte(line[i+1:], '\x1f'); j > 0 {
			start := b.utf16Pos
			b.parseInline(line[i+1 : i+1+j])
			b.addFormat(start, b.utf16Pos, protoFormatUnderline)

			return i + 1 + j + 1, true
		}

		return i, false

	case '*', '_', '~':
		return b.tryInlineStyle(line, i)

	case '@':
		return b.tryMention(line, i)

	default:
		return i, false
	}
}

func (b *outgoingBuilder) tryInlineStyle(line string, i int) (int, bool) {
	switch line[i] {
	case '*':
		if end, ok := findInlineDelim(line, i, "***", false); ok {
			start := b.utf16Pos
			b.parseInline(line[i+3 : end])
			b.addFormat(start, b.utf16Pos, protoFormatBold)
			b.addFormat(start, b.utf16Pos, protoFormatItalic)

			return end + 3, true
		}

		if end, ok := findInlineDelim(line, i, "**", false); ok {
			start := b.utf16Pos
			b.parseInline(line[i+2 : end])
			b.addFormat(start, b.utf16Pos, protoFormatBold)

			return end + 2, true
		}

		if end, ok := findInlineDelim(line, i, "*", false); ok {
			start := b.utf16Pos
			b.parseInline(line[i+1 : end])
			b.addFormat(start, b.utf16Pos, protoFormatItalic)

			return end + 1, true
		}

	case '_':
		if end, ok := findInlineDelim(line, i, "_", true); ok {
			start := b.utf16Pos
			b.parseInline(line[i+1 : end])
			b.addFormat(start, b.utf16Pos, protoFormatItalic)

			return end + 1, true
		}

	case '~':
		if end, ok := findInlineDelim(line, i, "~~", false); ok {
			start := b.utf16Pos
			b.parseInline(line[i+2 : end])
			b.addFormat(start, b.utf16Pos, protoFormatStrike)

			return end + 2, true
		}
	}

	return i, false
}

func (b *outgoingBuilder) tryMention(line string, i int) (int, bool) {
	if i > 0 && !isMentionBoundary(line[i-1]) {
		return i, false
	}

	token, nextIdx := scanMentionToken(line, i+1)
	if token == "" {
		return i, false
	}

	if strings.EqualFold(token, "all") {
		start := b.utf16Pos
		b.writeString(line[i:nextIdx])
		b.addMention(start, b.utf16Pos, "", "", true)

		return nextIdx, true
	}

	if b.resolveRef == nil {
		return i, false
	}

	uid, display := b.resolveRef(token)
	if uid == "" {
		return i, false
	}

	label := strings.TrimSpace(display)
	if label == "" {
		label = token
	}

	start := b.utf16Pos
	b.writeString("@" + label)
	b.addMention(start, b.utf16Pos, uid, label, false)

	return nextIdx, true
}

func findInlineDelim(s string, pos int, delim string, wordBoundary bool) (int, bool) {
	dLen := len(delim)
	dByte := delim[0]

	if !isValidDelimOpen(s, pos, delim, wordBoundary) {
		return 0, false
	}

	innerStart := pos + dLen

	for search := innerStart; search < len(s); {
		rel := strings.Index(s[search:], delim)
		if rel < 0 {
			return 0, false
		}

		closeIdx := search + rel
		afterIdx := closeIdx + dLen

		if isValidDelimClose(s, innerStart, closeIdx, afterIdx, dByte, wordBoundary) {
			return closeIdx, true
		}

		search = afterIdx
	}

	return 0, false
}

func isValidDelimOpen(s string, pos int, delim string, wordBoundary bool) bool {
	dByte := delim[0]
	innerStart := pos + len(delim)

	if !strings.HasPrefix(s[pos:], delim) || innerStart >= len(s) {
		return false
	}

	if pos > 0 && s[pos-1] == dByte {
		return false
	}

	if s[innerStart] == dByte || s[innerStart] == ' ' || s[innerStart] == '\t' {
		return false
	}

	return !wordBoundary || pos == 0 || !isASCIIWordByte(s[pos-1])
}

func isValidDelimClose(s string, innerStart, closeIdx, afterIdx int, dByte byte, wordBoundary bool) bool {
	if closeIdx == innerStart || s[closeIdx-1] == ' ' || s[closeIdx-1] == '\t' || s[closeIdx-1] == dByte {
		return false
	}

	if afterIdx < len(s) && s[afterIdx] == dByte {
		return false
	}

	return !wordBoundary || afterIdx >= len(s) || !isASCIIWordByte(s[afterIdx])
}

func scanMentionToken(s string, start int) (string, int) {
	end := start
	for end < len(s) && isMentionTokenByte(s[end]) {
		end++
	}

	// Trim trailing sentence punctuation like "." or "-" so "@alice." matches "alice".
	for end > start && (s[end-1] == '.' || s[end-1] == '-') {
		end--
	}

	if end == start {
		return "", start
	}

	if end < len(s) && !isMentionBoundary(s[end]) {
		return "", start
	}

	return s[start:end], end
}

func isMentionTokenByte(b byte) bool {
	return isASCIIWordByte(b) || b == '.' || b == '-'
}

func isASCIIWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

func isMentionBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', ',', '.', ':', ';', '!', '?', '(', ')', '[', ']', '*', '_', '~':
		return true
	default:
		return false
	}
}
