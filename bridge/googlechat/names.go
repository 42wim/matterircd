package googlechat

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"unicode"

	"github.com/matterbridge/googlechatclient"
)

const (
	maxIRCChannelNameLen = 64
	maxIRCNickLen        = 48
)

// rebuildNames computes deterministic, collision-free IRC channel names for all
// cached Google Chat Spaces and Group DMs, and IRC nicks for cached users.
func (g *GoogleChat) rebuildNames() {
	convs := g.gc.GetChannels()
	users := g.gc.GetUsers()

	g.namesMu.Lock()
	defer g.namesMu.Unlock()

	g.rebuildNicksLocked(users)
	g.rebuildChannelNamesLocked(convs)
}

func (g *GoogleChat) rebuildNicksLocked(users []*googlechatclient.User) {
	slices.SortFunc(users, func(a, b *googlechatclient.User) int {
		return strings.Compare(a.ID, b.ID)
	})

	nickByID := make(map[string]string, len(users))
	idByNick := make(map[string]string, len(users))

	for _, u := range users {
		if u == nil || u.ID == "" {
			continue
		}

		base := baseNickForUser(u)
		nick := deduplicateName(base, u.ID, idByNick, maxIRCNickLen)

		nickByID[u.ID] = nick
		idByNick[strings.ToLower(nick)] = u.ID
	}

	g.nickByID = nickByID
	g.idByNick = idByNick
}

func (g *GoogleChat) rebuildChannelNamesLocked(convs []*googlechatclient.Conversation) {
	slices.SortFunc(convs, func(a, b *googlechatclient.Conversation) int {
		return strings.Compare(a.ID, b.ID)
	})

	chanByID := make(map[string]string, len(convs))
	idByChan := make(map[string]string, len(convs))

	for _, conv := range convs {
		if conv == nil || conv.ID == "" {
			continue
		}

		if conv.Type == googlechatclient.ConversationDM || conv.Type == googlechatclient.ConversationSelfNote {
			chanByID[conv.ID] = conv.ID
			idByChan[strings.ToLower(conv.ID)] = conv.ID

			continue
		}

		base := g.baseChannelNameLocked(conv)
		name := deduplicateName(base, conv.ID, idByChan, maxIRCChannelNameLen)

		chanByID[conv.ID] = name
		idByChan[strings.ToLower(name)] = conv.ID
	}

	g.chanByID = chanByID
	g.idByChan = idByChan
}

func (g *GoogleChat) baseChannelNameLocked(conv *googlechatclient.Conversation) string {
	// Named Spaces and Group DMs use #<sanitized-display-name>; unnamed ones
	// (including modern Google Chat group chats stored as unnamed Spaces) use
	// #<member1-member2-...> matching Mattermost's group-DM channel naming.
	if slug := sanitizeIRCChannelPart(conv.DisplayName); slug != "" {
		return slug
	}

	return g.groupMembersSlugLocked(conv)
}

func (g *GoogleChat) groupMembersSlugLocked(conv *googlechatclient.Conversation) string {
	parts := make([]string, 0, len(conv.Members))

	for _, uid := range conv.Members {
		if uid == "" {
			continue
		}

		nick := g.nickByID[uid]
		if nick == "" {
			if u := g.gc.GetUser(uid); u != nil {
				nick = baseNickForUser(u)
			} else {
				nick = shortHash(uid)
			}
		}

		slug := sanitizeIRCChannelPart(nick)
		if slug != "" {
			parts = append(parts, slug)
		}
	}

	if len(parts) == 0 {
		return shortHash(conv.ID)
	}

	slices.Sort(parts)

	return strings.Join(parts, "-")
}

func (g *GoogleChat) ircChannelName(convID string) string {
	g.namesMu.RLock()
	name := g.chanByID[convID]
	g.namesMu.RUnlock()

	if name != "" {
		return name
	}

	g.rebuildNames()

	g.namesMu.RLock()
	name = g.chanByID[convID]
	g.namesMu.RUnlock()

	if name != "" {
		return name
	}

	return convID
}

func (g *GoogleChat) conversationIDByName(name string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(name), "#")
	if trimmed == "" {
		return ""
	}

	g.namesMu.RLock()
	id := g.idByChan[strings.ToLower(trimmed)]
	g.namesMu.RUnlock()

	if id != "" {
		return id
	}

	if g.gc.GetChannel(trimmed) != nil {
		return trimmed
	}

	return ""
}

func (g *GoogleChat) nickForUserID(userID string) string {
	if userID == "" {
		return ""
	}

	g.namesMu.RLock()
	nick := g.nickByID[userID]
	g.namesMu.RUnlock()

	if nick != "" {
		return nick
	}

	if u := g.gc.GetUser(userID); u != nil {
		base := baseNickForUser(u)

		g.namesMu.Lock()
		if existing := g.nickByID[userID]; existing != "" {
			g.namesMu.Unlock()

			return existing
		}

		nick = deduplicateName(base, userID, g.idByNick, maxIRCNickLen)
		g.nickByID[userID] = nick
		g.idByNick[strings.ToLower(nick)] = userID
		g.namesMu.Unlock()

		return nick
	}

	return "user-" + shortHash(userID)
}

func (g *GoogleChat) userIDForNick(nick string) string {
	if nick == "" {
		return ""
	}

	g.namesMu.RLock()
	uid := g.idByNick[strings.ToLower(nick)]
	g.namesMu.RUnlock()

	return uid
}

func baseNickForUser(u *googlechatclient.User) string {
	if u.Email != "" {
		local, _, _ := strings.Cut(u.Email, "@")
		if nick := sanitizeIRCNick(local); nick != "" {
			return nick
		}
	}

	if nick := sanitizeIRCNick(u.DisplayName); nick != "" {
		return nick
	}

	return "user-" + shortHash(u.ID)
}

func sanitizeIRCNick(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var b strings.Builder

	b.Grow(len(raw))

	lastSep := false
	for _, r := range raw {
		lastSep = writeSanitizedNickRune(&b, r, lastSep)
	}

	nick := strings.TrimRight(b.String(), ".-_")
	if nick == "" {
		return ""
	}

	if nick[0] >= '0' && nick[0] <= '9' {
		nick = "u" + nick
	}

	if len(nick) > maxIRCNickLen {
		nick = nick[:maxIRCNickLen]
	}

	return nick
}

func writeSanitizedNickRune(b *strings.Builder, r rune, lastSep bool) bool {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		b.WriteRune(unicode.ToLower(r))

		return false
	}

	if b.Len() == 0 || lastSep {
		return lastSep
	}

	switch {
	case r == '.' || r == '-' || r == '_':
		b.WriteRune(r)

		return true
	case unicode.IsSpace(r):
		b.WriteByte('.')

		return true
	default:
		return lastSep
	}
}

func sanitizeIRCChannelPart(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var b strings.Builder

	b.Grow(len(raw))

	lastDash := false

	for _, r := range raw {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))

			lastDash = false
		case r == '_' || r == '.':
			if b.Len() > 0 && !lastDash {
				b.WriteRune(r)

				lastDash = true
			}
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')

				lastDash = true
			}
		}
	}

	return strings.Trim(b.String(), "-_.")
}

func deduplicateName(base, uniqueKey string, used map[string]string, maxLen int) string {
	if len(base) > maxLen {
		base = strings.TrimRight(base[:maxLen], "-_.")
	}

	if base == "" {
		base = shortHash(uniqueKey)
	}

	lower := strings.ToLower(base)
	if existing, taken := used[lower]; !taken || existing == uniqueKey {
		return base
	}

	suffix := "-" + shortHash(uniqueKey)
	prefixMax := max(maxLen-len(suffix), 1)

	prefix := base
	if len(prefix) > prefixMax {
		prefix = strings.TrimRight(prefix[:prefixMax], "-_.")
	}

	return prefix + suffix
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:2])
}
