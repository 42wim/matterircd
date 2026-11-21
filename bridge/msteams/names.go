package msteams

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"unicode"

	"github.com/matterbridge/msteamsclient"
)

const (
	maxIRCChannelNameLen = 64
	maxIRCNickLen        = 48
	groupChannelPrefix   = "group-"
)

// rebuildNames computes deterministic, collision-free IRC channel names for all
// cached Teams channels and group chats, and IRC nicks for cached users.
func (m *MSTeams) rebuildNames() {
	convs := m.mc.GetChannels()
	users := m.mc.GetUsers()
	teams := m.mc.GetTeams()
	prefixMain := m.cfg.MSTeams().PrefixMainTeam

	m.namesMu.Lock()
	defer m.namesMu.Unlock()

	m.rebuildNicksLocked(users)
	m.rebuildChannelNamesLocked(convs, teams, prefixMain)
}

func (m *MSTeams) rebuildNicksLocked(users []*msteamsclient.User) {
	slices.SortFunc(users, func(a, b *msteamsclient.User) int {
		return strings.Compare(a.MRI, b.MRI)
	})

	nickByMRI := make(map[string]string, len(users))
	mriByNick := make(map[string]string, len(users))

	for _, u := range users {
		if u == nil || u.MRI == "" {
			continue
		}

		base := baseNickForUser(u)
		nick := deduplicateName(base, u.MRI, mriByNick, maxIRCNickLen)

		nickByMRI[u.MRI] = nick
		mriByNick[strings.ToLower(nick)] = u.MRI
	}

	m.nickByMRI = nickByMRI
	m.mriByNick = mriByNick
}

func (m *MSTeams) rebuildChannelNamesLocked(convs []*msteamsclient.Conversation, teams []*msteamsclient.Team, prefixMain bool) {
	slices.SortFunc(convs, func(a, b *msteamsclient.Conversation) int {
		return strings.Compare(a.ID, b.ID)
	})

	primaryTeamID := ""

	if len(teams) > 0 {
		slices.SortFunc(teams, func(a, b *msteamsclient.Team) int {
			return strings.Compare(a.ID, b.ID)
		})
		primaryTeamID = teams[0].ID
	}

	multiTeam := len(teams) > 1

	chanByID := make(map[string]string, len(convs))
	idByChan := make(map[string]string, len(convs))

	for _, conv := range convs {
		if conv == nil || conv.ID == "" {
			continue
		}

		if conv.Type == msteamsclient.ConversationDM || conv.Type == msteamsclient.ConversationSelfNote {
			chanByID[conv.ID] = conv.ID
			idByChan[strings.ToLower(conv.ID)] = conv.ID

			continue
		}

		base := m.baseChannelNameLocked(conv, primaryTeamID, multiTeam, prefixMain)
		name := deduplicateName(base, conv.ID, idByChan, maxIRCChannelNameLen)

		chanByID[conv.ID] = name
		idByChan[strings.ToLower(name)] = conv.ID
	}

	m.chanByID = chanByID
	m.idByChan = idByChan
}

func (m *MSTeams) baseChannelNameLocked(conv *msteamsclient.Conversation, primaryTeamID string, multiTeam, prefixMain bool) string {
	switch conv.Type {
	case msteamsclient.ConversationChannel:
		chSlug := sanitizeIRCChannelPart(conv.DisplayName)
		if chSlug == "" {
			chSlug = shortHash(conv.ID)
		}

		teamSlug := sanitizeIRCChannelPart(conv.TeamName)
		if teamSlug != "" && (prefixMain || multiTeam || (primaryTeamID != "" && conv.TeamID != primaryTeamID)) {
			return teamSlug + "-" + chSlug
		}

		return chSlug

	default:
		// Group and meeting chats use #group-<topic-or-members>.
		topicSlug := sanitizeIRCChannelPart(conv.Topic)
		if topicSlug == "" {
			topicSlug = sanitizeIRCChannelPart(conv.DisplayName)
		}

		if topicSlug != "" {
			return groupChannelPrefix + topicSlug
		}

		return groupChannelPrefix + m.groupMembersSlugLocked(conv)
	}
}

func (m *MSTeams) groupMembersSlugLocked(conv *msteamsclient.Conversation) string {
	me := m.mc.Me()

	myMRI := ""
	if me != nil {
		myMRI = me.MRI
	}

	parts := make([]string, 0, 3)

	for _, mri := range conv.Members {
		if mri == "" || mri == myMRI {
			continue
		}

		nick := m.nickByMRI[mri]
		if nick == "" {
			if u := m.mc.GetUser(mri); u != nil {
				nick = baseNickForUser(u)
			} else {
				nick = shortHash(mri)
			}
		}

		slug := sanitizeIRCChannelPart(nick)
		if slug != "" {
			parts = append(parts, slug)
		}

		if len(parts) == 3 {
			break
		}
	}

	if len(parts) == 0 {
		return shortHash(conv.ID)
	}

	return strings.Join(parts, "-")
}

func (m *MSTeams) ircChannelName(convID string) string {
	m.namesMu.RLock()
	name := m.chanByID[convID]
	m.namesMu.RUnlock()

	if name != "" {
		return name
	}

	// Lazily compute if a new conversation arrived since the last sync.
	m.rebuildNames()

	m.namesMu.RLock()
	name = m.chanByID[convID]
	m.namesMu.RUnlock()

	if name != "" {
		return name
	}

	return convID
}

func (m *MSTeams) conversationIDByName(name string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(name), "#")
	if trimmed == "" {
		return ""
	}

	m.namesMu.RLock()
	id := m.idByChan[strings.ToLower(trimmed)]
	m.namesMu.RUnlock()

	if id != "" {
		return id
	}

	if m.mc.GetChannel(trimmed) != nil {
		return trimmed
	}

	return ""
}

func (m *MSTeams) nickForMRI(mri string) string {
	if mri == "" {
		return ""
	}

	m.namesMu.RLock()
	nick := m.nickByMRI[mri]
	m.namesMu.RUnlock()

	if nick != "" {
		return nick
	}

	if u := m.mc.GetUser(mri); u != nil {
		base := baseNickForUser(u)

		m.namesMu.Lock()
		if existing := m.nickByMRI[mri]; existing != "" {
			m.namesMu.Unlock()

			return existing
		}

		nick = deduplicateName(base, mri, m.mriByNick, maxIRCNickLen)
		m.nickByMRI[mri] = nick
		m.mriByNick[strings.ToLower(nick)] = mri
		m.namesMu.Unlock()

		return nick
	}

	return "user-" + shortHash(mri)
}

func (m *MSTeams) mriForNick(nick string) string {
	if nick == "" {
		return ""
	}

	m.namesMu.RLock()
	mri := m.mriByNick[strings.ToLower(nick)]
	m.namesMu.RUnlock()

	return mri
}

func baseNickForUser(u *msteamsclient.User) string {
	if u.Email != "" {
		local, _, _ := strings.Cut(u.Email, "@")
		if nick := sanitizeIRCNick(local); nick != "" {
			return nick
		}
	}

	if u.UPN != "" {
		local, _, _ := strings.Cut(u.UPN, "@")
		if nick := sanitizeIRCNick(local); nick != "" {
			return nick
		}
	}

	if nick := sanitizeIRCNick(u.DisplayName); nick != "" {
		return nick
	}

	return "user-" + shortHash(u.MRI)
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
