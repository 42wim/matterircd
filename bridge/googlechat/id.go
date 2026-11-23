package googlechat

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	googleChatIDPrefix   = "gc"
	googleChatRawIDLen   = 11
	googleChatTotalIDLen = len(googleChatIDPrefix) + googleChatRawIDLen // 13: gc[0-9A-Za-z_-]{11}
)

// isValidGoogleChatRawID reports whether s is a valid 11-character Google Chat
// message/topic identifier ([0-9A-Za-z_-]{11}).
func isValidGoogleChatRawID(s string) bool {
	if len(s) != googleChatRawIDLen {
		return false
	}

	for i := range len(s) {
		c := s[i]
		if !isGoogleChatIDChar(c) {
			return false
		}
	}

	return true
}

func isGoogleChatIDChar(c byte) bool {
	return (c >= '0' && c <= '9') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') ||
		c == '_' || c == '-'
}

// encodeGoogleChatID prefixes a native 11-character Google Chat message/topic ID
// with "gc" to produce a 13-character identifier (gc[0-9A-Za-z_-]{11}) that is
// unambiguous against Mattermost (26 chars), MS Teams (mt...), Rocket.Chat (17),
// Slack (sl...), and Matrix ($...).
func encodeGoogleChatID(rawID string) string {
	trimmed := strings.TrimSpace(rawID)
	if trimmed == "" {
		return ""
	}

	if len(trimmed) == googleChatTotalIDLen && strings.HasPrefix(trimmed, googleChatIDPrefix) &&
		isValidGoogleChatRawID(trimmed[len(googleChatIDPrefix):]) {
		return trimmed
	}

	if isValidGoogleChatRawID(trimmed) {
		return googleChatIDPrefix + trimmed
	}

	return trimmed
}

// decodeGoogleChatID strips the "gc" prefix from a gc[0-9A-Za-z_-]{11} identifier
// and returns the native 11-character Google Chat ID (preserving exact case).
func decodeGoogleChatID(encoded string) (string, bool) {
	trimmed := strings.TrimSpace(encoded)
	if len(trimmed) != googleChatTotalIDLen || !strings.HasPrefix(trimmed, googleChatIDPrefix) {
		return "", false
	}

	raw := trimmed[len(googleChatIDPrefix):]
	if !isValidGoogleChatRawID(raw) {
		return "", false
	}

	return raw, true
}

// parseGoogleChatMessageURL extracts (convID, topicID, msgID, isDM) from Google Chat
// web links:
//   - https://chat.google.com/room/<spaceID>/<topicID>[/<msgID>]
//   - https://chat.google.com/u/<n>/room/<spaceID>/<topicID>[/<msgID>]
//   - https://chat.google.com/dm/<dmID>/<topicID>[/<msgID>]
//   - https://chat.google.com/u/<n>/dm/<dmID>/<topicID>[/<msgID>]
//   - https://mail.google.com/chat/u/<n>/#chat/space/<spaceID>/<topicID>[/<msgID>]
//   - https://mail.google.com/chat/u/<n>/#chat/dm/<dmID>/<topicID>[/<msgID>]
func parseGoogleChatMessageURL(raw string) (string, string, string, bool, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", "", false, false
	}

	host := strings.ToLower(u.Hostname())
	switch host {
	case "chat.google.com":
		return parseChatHostPath(u.Path)
	case "mail.google.com":
		return parseMailChatFragment(u.Fragment)
	default:
		return "", "", "", false, false
	}
}

func parseChatHostPath(p string) (string, string, string, bool, bool) {
	parts := splitNonEmptyPath(p)
	if len(parts) >= 2 && parts[0] == "u" {
		parts = parts[2:]
	}

	return extractChatPathTuple(parts)
}

func parseMailChatFragment(frag string) (string, string, string, bool, bool) {
	parts := splitNonEmptyPath(frag)
	if len(parts) > 0 && parts[0] == "chat" {
		parts = parts[1:]
	}

	return extractChatPathTuple(parts)
}

func extractChatPathTuple(parts []string) (string, string, string, bool, bool) {
	if len(parts) < 3 {
		return "", "", "", false, false
	}

	var isDM bool

	switch parts[0] {
	case "room", "space":
		isDM = false
	case "dm":
		isDM = true
	default:
		return "", "", "", false, false
	}

	convID := parts[1]
	topicID := parts[2]

	msgID := topicID
	if len(parts) >= 4 && isValidGoogleChatRawID(parts[3]) {
		msgID = parts[3]
	}

	if convID == "" || !isValidGoogleChatRawID(topicID) {
		return "", "", "", false, false
	}

	return convID, topicID, msgID, isDM, true
}

func splitNonEmptyPath(p string) []string {
	var out []string

	for part := range strings.SplitSeq(p, "/") {
		if part != "" {
			out = append(out, part)
		}
	}

	return out
}

func googleChatMessageURL(authUser int, convID, topicID, msgID string, isDM bool) string {
	if convID == "" {
		return ""
	}

	if topicID == "" {
		topicID = msgID
	}

	if topicID == "" {
		return ""
	}

	base := "https://chat.google.com"
	if authUser > 0 {
		base = fmt.Sprintf("%s/u/%d", base, authUser)
	}

	kind := "room"
	if isDM {
		kind = "dm"
	}

	if msgID != "" && msgID != topicID {
		return base + "/" + kind + "/" + url.PathEscape(convID) + "/" + url.PathEscape(topicID) + "/" + url.PathEscape(msgID)
	}

	return base + "/" + kind + "/" + url.PathEscape(convID) + "/" + url.PathEscape(topicID)
}
