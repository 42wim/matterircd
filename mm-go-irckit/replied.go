package irckit

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/42wim/matterircd/bridge"
)

const (
	defaultRepliedThreadCacheSize = 100
	repliedModeOff                = "off"
	repliedModeFalse              = "false"
	repliedModeTrue               = "true"
	repliedModeNick               = "nick"
	eventReaction                 = "reaction"
	eventPostDeleted              = "post_deleted"
	eventChannelPurpose           = "channel_purpose"
	eventDMTopic                  = "dm_topic"
)

func (u *User) repliedThreadCacheSize() int {
	if u.hasSharedCfg() && u.sharedCfg().RepliedThreadsCacheSize > 0 {
		return u.sharedCfg().RepliedThreadsCacheSize
	}

	return defaultRepliedThreadCacheSize
}

func threadOrMsgID(parentID, msgID string) string {
	return cmp.Or(parentID, msgID)
}

func repliedThreadsKey(proto string) []byte {
	return []byte("__replied_threads_" + proto + "__")
}

func (u *User) repliedHighlightTag() string {
	if !u.hasSharedCfg() {
		return ""
	}

	mode := strings.TrimSpace(u.sharedCfg().HighlightRepliedThreads)
	switch strings.ToLower(mode) {
	case "", repliedModeOff, repliedModeFalse:
		return ""
	case threadColorModeOn, repliedModeTrue, repliedModeNick:
		if u.Nick == "" {
			return ""
		}

		return "@" + u.Nick
	default:
		return mode
	}
}

func (u *User) addRepliedThread(channelID, threadID string) {
	if channelID == "" || threadID == "" || u.repliedHighlightTag() == "" {
		return
	}

	u.repliedMu.Lock()
	defer u.repliedMu.Unlock()

	if u.repliedThreads == nil {
		u.repliedThreads = make(map[string][]string)
	}

	ids := u.repliedThreads[channelID]
	if idx := slices.Index(ids, threadID); idx >= 0 {
		if idx == len(ids)-1 {
			return
		}

		ids = slices.Delete(ids, idx, idx+1)
	}

	maxSize := u.repliedThreadCacheSize()

	ids = append(ids, threadID)
	if len(ids) > maxSize {
		ids = slices.Clone(ids[len(ids)-maxSize:])
	}

	u.repliedThreads[channelID] = ids
	u.repliedDirty = true
}

func (u *User) isRepliedThread(channelID, threadID string) bool {
	if channelID == "" || threadID == "" {
		return false
	}

	u.repliedMu.Lock()
	defer u.repliedMu.Unlock()

	return slices.Contains(u.repliedThreads[channelID], threadID)
}

func (u *User) formatHighlightSuffix(tag string) string {
	if u.br != nil && u.br.FormatterConfig().Unicode {
		return " (\U0001F514 " + tag + ")"
	}

	return " (re " + tag + ")"
}

func (u *User) formatMentionSuffix(nick string) string {
	if u.br != nil && u.br.FormatterConfig().Unicode {
		return " (\U0001F514 " + nick + ")"
	}

	return " (@" + nick + ")"
}

func (u *User) textAlreadyHighlights(text, tag string) bool {
	if strings.Contains(text, tag) {
		return true
	}

	if u.Nick != "" && (strings.Contains(text, "@"+u.Nick) || strings.Contains(text, u.formatMentionSuffix(u.Nick))) {
		return true
	}

	for _, m := range u.MentionKeys {
		if m != "" && strings.Contains(text, m) {
			return true
		}
	}

	return false
}

func (u *User) applyRepliedHighlight(event *bridge.ChannelMessageEvent) {
	tag := u.repliedHighlightTag()
	if tag == "" || event == nil || event.ParentID == "" || event.ChannelType == "D" {
		return
	}

	if event.Sender == nil || event.Sender.Me || event.Sender.Nick == systemUser {
		return
	}

	switch event.Event {
	case "post_edited", eventPostDeleted, eventReaction, eventChannelPurpose:
		return
	}

	if !u.isRepliedThread(event.ChannelID, event.ParentID) || u.textAlreadyHighlights(event.Text, tag) {
		return
	}

	if before, after, ok := strings.Cut(event.Text, " (re @"); ok {
		event.Text = before + " (re " + tag + ", @" + after
		return
	}

	event.Text += u.formatHighlightSuffix(tag)
}

func (u *User) loadRepliedThreads(protocol string) {
	db := u.getDB(protocol)
	if db == nil {
		return
	}

	var raw []byte

	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(u.dbBucketName())
		if b != nil {
			if v := b.Get(repliedThreadsKey(protocol)); len(v) > 0 {
				raw = bytes.Clone(v)
			}
		}

		return nil
	})

	if len(raw) == 0 {
		return
	}

	var loaded map[string][]string

	err := json.Unmarshal(raw, &loaded)
	if err != nil {
		logger.Debugf("replied threads load failed: %v", err)
		return
	}

	maxSize := u.repliedThreadCacheSize()

	for chID, ids := range loaded {
		if len(ids) > maxSize {
			loaded[chID] = slices.Clone(ids[len(ids)-maxSize:])
		}
	}

	u.repliedMu.Lock()
	u.repliedThreads = loaded
	u.repliedDirty = false
	u.repliedMu.Unlock()
}

func (u *User) flushRepliedThreads() {
	u.repliedMu.Lock()

	if !u.repliedDirty {
		u.repliedMu.Unlock()
		return
	}

	snap := make(map[string][]string, len(u.repliedThreads))
	for chID, ids := range u.repliedThreads {
		if len(ids) > 0 {
			snap[chID] = slices.Clone(ids)
		}
	}

	u.repliedDirty = false
	u.repliedMu.Unlock()

	db := u.getDB()
	if db == nil {
		return
	}

	proto := u.currentProtocol()
	if proto == "" {
		return
	}

	data, err := json.Marshal(snap)
	if err != nil {
		logger.Debugf("replied threads marshal failed: %v", err)
		return
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b, err2 := tx.CreateBucketIfNotExists(u.dbBucketName())
		if err2 != nil {
			return err2
		}

		return b.Put(repliedThreadsKey(proto), data)
	})
	if err != nil {
		logger.Debugf("replied threads flush failed: %v", err)

		u.repliedMu.Lock()
		u.repliedDirty = true
		u.repliedMu.Unlock()
	}
}

func (u *User) clearRepliedThreadsCache() {
	u.repliedMu.Lock()
	clear(u.repliedThreads)
	u.repliedDirty = false
	u.repliedMu.Unlock()
}

func (u *User) repliedChannelLabel(channelID string) string {
	if br := u.currentBridge(); br != nil {
		ch, err := br.GetChannel(u.ctx, channelID)
		if err == nil && ch != nil && ch.Name != "" {
			return "#" + ch.Name
		}
	}

	return channelID
}

func (u *User) resolveRepliedID(channelID, raw string) string {
	id := strings.TrimPrefix(raw, "@@")
	if len(id) == 3 {
		if resolved := u.getPostIDFromHex(channelID, id); resolved != "" {
			return resolved
		}
	}

	if br := u.currentBridge(); br != nil {
		if norm := br.NormalizePostID(id); norm != "" {
			return norm
		}
	}

	return id
}

func (u *User) repliedList(toUser *User, filterChannelID string) {
	u.repliedMu.Lock()

	snap := make(map[string][]string, len(u.repliedThreads))
	for chID, ids := range u.repliedThreads {
		if filterChannelID != "" && chID != filterChannelID {
			continue
		}

		if len(ids) > 0 {
			snap[chID] = slices.Clone(ids)
		}
	}

	u.repliedMu.Unlock()

	if len(snap) == 0 {
		u.MsgUser(toUser, "replied thread cache is empty")
		return
	}

	type entry struct {
		label string
		ids   []string
	}

	entries := make([]entry, 0, len(snap))
	for chID, ids := range snap {
		entries = append(entries, entry{label: u.repliedChannelLabel(chID), ids: ids})
	}

	slices.SortFunc(entries, func(a, b entry) int {
		return strings.Compare(a.label, b.label)
	})

	colorMode := u.br.BridgeConfig().ThreadContextColor

	for _, e := range entries {
		formatted := make([]string, len(e.ids))
		for i, id := range e.ids {
			formatted[i] = formatSingleThreadContext(colorMode, "@@", id, id)
		}

		u.MsgUser(toUser, fmt.Sprintf("%s (%d): %s", e.label, len(e.ids), strings.Join(formatted, " ")))
	}
}

func (u *User) repliedClear(toUser *User, args []string) {
	if len(args) == 0 || (len(args) == 1 && args[0] == "*") {
		u.repliedMu.Lock()

		total := 0
		for _, ids := range u.repliedThreads {
			total += len(ids)
		}

		clear(u.repliedThreads)
		u.repliedDirty = true
		u.repliedMu.Unlock()

		u.MsgUser(toUser, fmt.Sprintf("cleared %d replied thread(s)", total))

		return
	}

	var (
		targetChID string
		rawIDs     []string
	)

	if chName, ok := strings.CutPrefix(args[0], "#"); ok {
		targetChID = u.br.GetChannelID(u.ctx, chName, u.br.GetMe().TeamID)
		if targetChID == "" {
			u.MsgUser(toUser, args[0]+" not found")
			return
		}

		rawIDs = args[1:]
	} else {
		rawIDs = args
	}

	removed := u.removeRepliedThreads(targetChID, rawIDs)
	u.MsgUser(toUser, fmt.Sprintf("cleared %d replied thread(s)", removed))
}

func (u *User) removeRepliedThreads(targetChID string, rawIDs []string) int {
	u.repliedMu.Lock()
	defer u.repliedMu.Unlock()

	if targetChID != "" && len(rawIDs) == 0 {
		n := len(u.repliedThreads[targetChID])
		if n > 0 {
			delete(u.repliedThreads, targetChID)
			u.repliedDirty = true
		}

		return n
	}

	removed := 0

	for chID, ids := range u.repliedThreads {
		if targetChID != "" && chID != targetChID {
			continue
		}

		targets := make([]string, len(rawIDs))
		for i, r := range rawIDs {
			targets[i] = u.resolveRepliedID(chID, r)
		}

		kept := make([]string, 0, len(ids))
		for _, id := range ids {
			if slices.Contains(targets, id) {
				removed++
			} else {
				kept = append(kept, id)
			}
		}

		if len(kept) == 0 {
			delete(u.repliedThreads, chID)
		} else {
			u.repliedThreads[chID] = kept
		}
	}

	if removed > 0 {
		u.repliedDirty = true
	}

	return removed
}

func repliedCmd(u *User, toUser *User, args []string, _ string) {
	if len(args) > 0 && strings.EqualFold(args[0], helpArg) {
		u.MsgUser(toUser, "usage: REPLIED [#channel]")
		u.MsgUser(toUser, "       REPLIED CLEAR [* | #channel [id...] | <id...>]")

		return
	}

	if len(args) > 0 && strings.EqualFold(args[0], "clear") {
		u.repliedClear(toUser, args[1:])
		return
	}

	if len(args) == 0 {
		u.repliedList(toUser, "")
		return
	}

	if len(args) == 1 {
		if chName, ok := strings.CutPrefix(args[0], "#"); ok {
			channelID := u.br.GetChannelID(u.ctx, chName, u.br.GetMe().TeamID)
			if channelID == "" {
				u.MsgUser(toUser, args[0]+" not found")
				return
			}

			u.repliedList(toUser, channelID)

			return
		}
	}

	u.MsgUser(toUser, "usage: REPLIED [#channel] | REPLIED CLEAR [* | #channel [id...] | <id...>]")
}
