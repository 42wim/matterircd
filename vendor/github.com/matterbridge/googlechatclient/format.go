package googlechatclient

import (
	"net/url"
	"strings"
)

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
	if pm == nil || len(pm.Annotations) == 0 {
		return nil, nil, pm.TextBody
	}

	var (
		mentions    []Mention
		attachments []Attachment
	)

	text := pm.TextBody

	for _, ann := range pm.Annotations {
		c.applySingleAnnotation(ann, &mentions, &attachments, &text)
	}

	return mentions, attachments, text
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
