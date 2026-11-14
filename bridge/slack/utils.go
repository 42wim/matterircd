package slack

import (
	"fmt"
	"html"
	"strings"
	"time"
)

func isValidNick(s string) bool {
	/* IRC RFC ([0] - see below) mentions a limit of 9 chars for
	 * IRC nicks, but modern clients allow more than that. Let's
	 * use a "sane" big value, the triple of the spec.
	 */
	if len(s) < 1 || len(s) > 27 {
		return false
	}

	/* According to IRC RFC [0], the allowed chars to have as nick
	 * are: ( letter / special-'-' ).*( letter / digit / special ),
	 * where:
	 * letter = [a-z / A-Z]; digit = [0-9];
	 * special = [';', '[', '\', ']', '^', '_', '`', '{', '|', '}', '-']
	 *
	 * ASCII codes (decimal) for the allowed chars:
	 * letter = [65-90,97-122]; digit = [48-57]
	 * special = [59, 91-96, 123-125, 45]
	 * [0] RFC 2812 (tools.ietf.org/html/rfc2812)
	 */

	if s[0] != 59 && (s[0] < 65 || s[0] > 125) {
		return false
	}

	for i := 1; i < len(s); i++ {
		if s[i] != 45 && s[i] != 59 && (s[i] < 65 || s[i] > 125) {
			if s[i] < 48 || s[i] > 57 {
				return false
			}
		}
	}

	return true
}

func formatTS(unixts string) string {
	var targetts, targetus int64

	_, _ = fmt.Sscanf(unixts, "%d.%d", &targetts, &targetus)
	ts := time.Unix(targetts, targetus*1000)

	if ts.YearDay() != time.Now().YearDay() {
		return ts.Format("2.1. 15:04:05")
	}

	return ts.Format("15:04:05")
}

func (s *Slack) formatSlackToken(inner string) string {
	if inner == "" {
		return ""
	}

	switch inner[0] {
	case '@':
		id, _, _ := strings.Cut(inner[1:], "|")

		return "@" + s.sc.UserName(id)
	case '#':
		id, label, hasLabel := strings.Cut(inner[1:], "|")
		if hasLabel && label != "" {
			return "#" + label
		}

		return s.sc.GetChannelName(id)
	case '!':
		id, label, hasLabel := strings.Cut(inner[1:], "|")
		if hasLabel && label != "" {
			return "@" + strings.TrimPrefix(label, "@")
		}

		return "@" + id
	default:
		link, _, _ := strings.Cut(inner, "|")

		return link
	}
}

func (s *Slack) cleanupMessage(msg string) string {
	if !strings.ContainsRune(msg, '<') {
		return html.UnescapeString(msg)
	}

	var b strings.Builder

	b.Grow(len(msg))

	for i := 0; i < len(msg); {
		relStart := strings.IndexByte(msg[i:], '<')
		if relStart == -1 {
			b.WriteString(msg[i:])
			break
		}

		start := i + relStart
		b.WriteString(msg[i:start])

		relEnd := strings.IndexByte(msg[start+1:], '>')
		if relEnd == -1 {
			b.WriteString(msg[start:])
			break
		}

		inner := msg[start+1 : start+1+relEnd]
		if nextOpen := strings.IndexByte(inner, '<'); nextOpen != -1 {
			b.WriteString(msg[start : start+1+nextOpen])
			i = start + 1 + nextOpen

			continue
		}

		b.WriteString(s.formatSlackToken(inner))

		i = start + 1 + relEnd + 1
	}

	return html.UnescapeString(b.String())
}
