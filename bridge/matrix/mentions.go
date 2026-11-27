package matrix

import (
	"strings"

	"maunium.net/go/mautrix/id"
)

const nickCacheSize = 8192

func (m *Matrix) cacheNick(nick string, userID id.UserID) {
	if m.nickCache != nil && nick != "" {
		m.nickCache.Add(strings.ToLower(nick), userID)
	}
}

// resolveOutboundMentions converts IRC-sanitized Matrix nicks in outbound text
// back into canonical Matrix User IDs (@localpart:homeserver).
//
// Remote, escaped (^hh), and capped (^h...) nicks are resolved both with and
// without a leading '@'. Plain same-homeserver nicks require a leading '@'.
// Fenced code blocks, inline code, URLs, emails, and room mentions are skipped.
func (m *Matrix) resolveOutboundMentions(text string) string {
	sepPrefix, _ := nickAffixes(m.nickSep)
	if !strings.ContainsAny(text, "@^") && (sepPrefix == "" || !strings.Contains(text, sepPrefix)) {
		return text
	}

	var (
		b         strings.Builder
		inFence   bool
		lastWrite int
	)

	for start := 0; start < len(text); {
		end := len(text)
		next := len(text)

		if nl := strings.IndexByte(text[start:], '\n'); nl >= 0 {
			end = start + nl
			next = end + 1
		}

		line := text[start:end]
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") && (inFence || !strings.Contains(trimmed[3:], "`")) {
			inFence = !inFence
		} else if !inFence {
			if resolved, changed := m.resolveMentionsInLine(line, sepPrefix); changed {
				if b.Len() == 0 {
					b.Grow(len(text) + 32)
				}

				b.WriteString(text[lastWrite:start])
				b.WriteString(resolved)

				lastWrite = end
			}
		}

		start = next
	}

	if b.Len() == 0 {
		return text
	}

	b.WriteString(text[lastWrite:])

	return b.String()
}

func (m *Matrix) resolveMentionsInLine(line, sepPrefix string) (string, bool) {
	if !strings.ContainsAny(line, "@^") && (sepPrefix == "" || !strings.Contains(line, sepPrefix)) {
		return line, false
	}

	var (
		b         strings.Builder
		lastWrite int
	)

	for i := 0; i < len(line); {
		if next, skipped := skipCodeOrURL(line, i); skipped {
			i = next

			continue
		}

		if strings.IndexByte(" \t\r([{<\"'*~,;", line[i]) >= 0 || (i > 0 && strings.IndexByte(" \t\r([{<\"'*~,;_", line[i-1]) < 0) {
			i++

			continue
		}

		if mxid, tokEnd, ok := m.matchMentionAt(line, i, sepPrefix); ok {
			if b.Len() == 0 {
				b.Grow(len(line) + 32)
			}

			b.WriteString(line[lastWrite:i])
			b.WriteString(mxid)

			lastWrite = tokEnd
			i = tokEnd

			continue
		}

		i++
	}

	if b.Len() == 0 {
		return line, false
	}

	b.WriteString(line[lastWrite:])

	return b.String(), true
}

func skipCodeOrURL(line string, i int) (int, bool) {
	if line[i] == '`' {
		if end := strings.IndexByte(line[i+1:], '`'); end >= 0 {
			return i + end + 2, true
		}

		return i + 1, true
	}

	rem := line[i:]
	if strings.HasPrefix(rem, "https://") || strings.HasPrefix(rem, "http://") || strings.HasPrefix(rem, "mailto:") {
		if end := strings.IndexAny(rem, " \t\r)>\"'`"); end >= 0 {
			return i + end, true
		}

		return len(line), true
	}

	return i, false
}

func (m *Matrix) matchMentionAt(line string, start int, sepPrefix string) (string, int, bool) {
	hasAt := line[start] == '@'
	bodyStart := start

	if hasAt {
		bodyStart++
		if bodyStart >= len(line) || line[bodyStart] == '@' {
			return "", 0, false
		}
	}

	end := len(line)
	if rel := strings.IndexAny(line[bodyStart:], " \t\r`,\"'!?*~@"); rel >= 0 {
		end = bodyStart + rel
	}

	for cand := line[bodyStart:end]; cand != ""; cand = cand[:len(cand)-1] {
		if uid, ok := m.lookupMentionNick(cand, hasAt, sepPrefix); ok {
			tokEnd := bodyStart + len(cand)
			if mxid := uid.String(); line[start:tokEnd] != mxid {
				return mxid, tokEnd, true
			}

			return "", 0, false
		}

		if strings.IndexByte(":.;)}>]_", cand[len(cand)-1]) < 0 {
			break
		}
	}

	return "", 0, false
}

func (m *Matrix) lookupMentionNick(cand string, hasAt bool, sepPrefix string) (id.UserID, bool) {
	if hasAt {
		switch strings.ToLower(cand) {
		case "room", "channel", "all", "here", "everyone":
			return "", false
		}

		if m.nickSep != ":" && strings.ContainsRune(cand, ':') {
			return "", false
		}
	} else if (sepPrefix == "" || !strings.Contains(cand, sepPrefix)) && !strings.ContainsRune(cand, '^') {
		return "", false
	}

	return m.resolveUserIDByNick(cand)
}

func (m *Matrix) resolveUserIDByNick(nick string) (id.UserID, bool) {
	lookupKey := strings.ToLower(nick)
	if m.nickCache != nil {
		if uid, ok := m.nickCache.Get(lookupKey); ok {
			return uid, true
		}
	}

	if m.mc == nil {
		return "", false
	}

	_, myHomeserver, _ := m.mc.UserID().Parse()

	userID, found := m.mc.FindUser(func(userID id.UserID, _ string) bool {
		localpart, homeserver, err := userID.Parse()
		if err != nil {
			return false
		}

		return strings.EqualFold(m.userNick(localpart, homeserver, userID, myHomeserver), nick)
	})
	if !found {
		return "", false
	}

	m.cacheNick(nick, userID)

	return userID, true
}
