package slack

import (
	"html"
	"strings"
)

func sanitizeSlackNick(name string) string {
	return strings.Join(strings.Fields(name), ".")
}

func (s *Slack) formatSlackToken(inner string) string {
	if inner == "" {
		return ""
	}

	switch inner[0] {
	case '@':
		id, _, _ := strings.Cut(inner[1:], "|")

		if u := s.createUser(s.sc.GetUser(id)); u != nil && u.Nick != "" {
			return "@" + u.Nick
		}

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
		if ch, ts, ok := parseSlackArchiveURL(link); ok {
			return link + " (" + encodeSlackID(ch, ts) + ")"
		}

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
