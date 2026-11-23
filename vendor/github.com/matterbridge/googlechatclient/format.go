package googlechatclient

import (
	"cmp"
	"net/url"
	"slices"
	"strings"
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
	Type       uint64
	StartIndex int32
	Length     int32
	UserID     string
	MentionAll bool
	FormatType uint64
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

// buildOutgoingAnnotations detects @all mentions and basic inline/italic
// formatting spans when sending messages to Google Chat.
func buildOutgoingAnnotations(text string) (string, []outgoingAnnotation) {
	// Convert CTCP-style *italic action* text when the entire message is wrapped in single '*'.
	if len(text) > 2 && strings.HasPrefix(text, "*") && strings.HasSuffix(text, "*") && !strings.HasPrefix(text, "**") {
		inner := text[1 : len(text)-1]
		if !strings.Contains(inner, "*") {
			runeLen := int32(len([]rune(inner))) //nolint:gosec // message length is bounded <= 4096

			return inner, []outgoingAnnotation{
				{
					Type:       protoAnnotationFormatData,
					StartIndex: 0,
					Length:     runeLen,
					FormatType: protoFormatItalic,
				},
			}
		}
	}

	var anns []outgoingAnnotation

	// Detect standalone @all mentions using rune offsets.
	lower := strings.ToLower(text)
	searchFrom := 0

	for {
		idx := strings.Index(lower[searchFrom:], "@all")
		if idx < 0 {
			break
		}

		pos := searchFrom + idx
		end := pos + 4

		beforeOK := pos == 0 || isMentionBoundary(text[pos-1])
		afterOK := end == len(text) || isMentionBoundary(text[end])

		if beforeOK && afterOK {
			runeStart := int32(len([]rune(text[:pos]))) //nolint:gosec // bounded message length
			anns = append(anns, outgoingAnnotation{
				Type:       protoAnnotationUserMention,
				StartIndex: runeStart,
				Length:     4,
				MentionAll: true,
			})
		}

		searchFrom = end
	}

	return text, anns
}

func isMentionBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', ',', '.', ':', ';', '!', '?', '(', ')', '[', ']':
		return true
	default:
		return false
	}
}
