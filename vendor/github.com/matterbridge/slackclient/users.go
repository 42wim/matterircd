package slackclient

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	presenceActive = "active"
	presenceAway   = "away"
)

var errNoPresence = errors.New("slackclient: presence unavailable")

func (c *Client) InitUsers() {
	users, _ := c.sc.GetUsers()

	c.Lock()
	defer c.Unlock()

	for _, mmuser := range users {
		// do not add our own nick
		if mmuser.ID == c.info.User.ID {
			continue
		}

		c.users[mmuser.ID] = mmuser

		if mmuser.Presence != "" {
			c.presences[mmuser.ID] = presenceEntry{
				presence: slack.UserPresence{
					Presence: mmuser.Presence,
					Online:   mmuser.Presence == presenceActive,
				},
			}
		}
	}
}

func (c *Client) GetUsers() []slack.User {
	c.RLock()
	defer c.RUnlock()

	users := make([]slack.User, 0, len(c.users))
	for _, user := range c.users {
		users = append(users, user)
	}

	return users
}

func (c *Client) GetUser(userID string) *slack.User {
	c.RLock()

	if user, ok := c.users[userID]; ok {
		c.RUnlock()

		return &user
	}

	c.RUnlock()

	c.logger.Debugf("user %s not in cache, asking slack", userID)

	user, err := c.sc.GetUserInfo(userID)
	if err != nil {
		return nil
	}

	c.Lock()
	c.users[user.ID] = *user
	c.Unlock()

	return user
}

func (c *Client) GetUserInfo(userID string) (*slack.User, error) {
	return c.rtm.GetUserInfo(userID)
}

func (c *Client) GetMe() *slack.User {
	if c.info == nil || c.info.User == nil {
		return nil
	}

	myID := c.info.User.ID

	c.RLock()
	u, ok := c.users[myID]
	c.RUnlock()

	if ok {
		return &u
	}

	if c.sc == nil {
		return nil
	}

	me, err := c.sc.GetUserInfo(myID)
	if err != nil || me == nil {
		return nil
	}

	c.Lock()
	c.users[myID] = *me
	c.Unlock()

	return me
}

func (c *Client) GetUserFromMessage(rmsg *slack.MessageEvent) (*slack.User, error) {
	usr := rmsg.User
	if rmsg.SubType == "message_changed" {
		usr = rmsg.SubMessage.User
	}

	if rmsg.SubType == "message_deleted" {
		usr = "USLACKBOT"
	}

	if rmsg.SubType == "bot_message" {
		suser := &slack.User{
			ID:     rmsg.BotID,
			TeamID: c.info.Team.ID,
			Profile: slack.UserProfile{
				FirstName: "bot", //nolint:goconst
				LastName:  "bot",
				RealName:  "bot",
			},
			Name: rmsg.Username,
		}

		if rmsg.Username == "" {
			bot, err := c.rtm.GetBotInfo(slack.GetBotInfoParameters{Bot: rmsg.BotID})
			if err != nil {
				suser.Profile.DisplayName = "bot"
				suser.Name = "bot"
			} else {
				suser.Profile.DisplayName = bot.Name
				suser.Name = bot.Name
			}
		}

		return suser, nil
	}

	suser, err := c.rtm.GetUserInfo(usr)
	if err != nil {
		return nil, err
	}

	return suser, nil
}

func (c *Client) UserName(id string) string {
	c.RLock()
	defer c.RUnlock()

	// TODO dynamically update when new users are joining slack
	for _, us := range c.users {
		if us.ID != id {
			continue
		}

		if us.Profile.DisplayName != "" {
			return us.Profile.DisplayName
		}

		return us.Name
	}

	if id == c.info.User.ID {
		return c.info.User.Name
	}

	return ""
}

func (c *Client) SetStatus(status string) error {
	switch status {
	case "online":
		return c.sc.SetUserPresence("auto")
	case presenceAway:
		return c.sc.SetUserPresence(presenceAway)
	}

	return nil
}

func (c *Client) UserID() string {
	if c.info == nil || c.info.User == nil {
		return ""
	}

	return c.info.User.ID
}

func (c *Client) CurrentUserName() string {
	if c.info == nil || c.info.User == nil {
		return ""
	}

	return c.info.User.Name
}

func (c *Client) TeamID() string {
	if c.info == nil || c.info.Team == nil {
		return ""
	}

	return c.info.Team.ID
}

func (c *Client) TeamName() string {
	if c.info == nil || c.info.Team == nil {
		return ""
	}

	return c.info.Team.Name
}

func (c *Client) TeamDomain() string {
	if c.info == nil || c.info.Team == nil {
		return ""
	}

	return c.info.Team.Domain
}

func (c *Client) SearchUsers(query string) []slack.User {
	c.RLock()
	empty := len(c.users) == 0
	c.RUnlock()

	if empty {
		c.InitUsers()
	}

	q := strings.ToLower(strings.TrimSpace(query))

	c.RLock()
	defer c.RUnlock()

	var matches []slack.User

	for _, u := range c.users {
		if q == "" ||
			strings.Contains(strings.ToLower(u.Name), q) ||
			strings.Contains(strings.ToLower(u.RealName), q) ||
			strings.Contains(strings.ToLower(u.Profile.RealName), q) ||
			strings.Contains(strings.ToLower(u.Profile.DisplayName), q) ||
			strings.Contains(strings.ToLower(u.Profile.Email), q) {
			matches = append(matches, u)
		}
	}

	return matches
}

func (c *Client) GetUserCount() int {
	c.RLock()
	defer c.RUnlock()

	return len(c.users)
}

func (c *Client) GetUserByUsername(username string) *slack.User {
	if username == "" {
		return nil
	}

	c.RLock()
	empty := len(c.users) == 0
	c.RUnlock()

	if empty && c.sc != nil {
		c.InitUsers()
	}

	c.RLock()
	defer c.RUnlock()

	for _, u := range c.users {
		if strings.EqualFold(u.Name, username) || strings.EqualFold(u.Profile.DisplayName, username) {
			user := u
			return &user
		}
	}

	return nil
}

func (c *Client) updateSelfDisplayName(name string) {
	if c.info == nil || c.info.User == nil || c.info.User.ID == "" {
		return
	}

	myID := c.info.User.ID

	c.Lock()
	defer c.Unlock()

	u, ok := c.users[myID]
	if !ok {
		u = slack.User{
			ID:   myID,
			Name: c.info.User.Name,
		}
	}

	u.Profile.DisplayName = name
	u.Profile.DisplayNameNormalized = name
	c.users[myID] = u
}

func (c *Client) SetNick(ctx context.Context, name string) error {
	if c.sc == nil {
		return errors.New("slackclient: not connected")
	}

	err := c.sc.SetUserProfileContext(ctx, "", &slack.UserProfile{
		DisplayName: name,
	})
	if err != nil {
		return err
	}

	c.updateSelfDisplayName(name)

	return nil
}

const (
	presenceCacheTTL = 30 * time.Second
	presenceStaleTTL = 5 * time.Minute
)

func mergeSlackUser(existing, updated slack.User) slack.User {
	if updated.Name == "" {
		updated.Name = existing.Name
	}

	if updated.RealName == "" {
		if updated.Profile.RealName != "" {
			updated.RealName = updated.Profile.RealName
		} else {
			updated.RealName = existing.RealName
		}
	}

	if updated.TeamID == "" {
		updated.TeamID = existing.TeamID
	}

	if updated.TZ == "" {
		updated.TZ = existing.TZ
		updated.TZLabel = existing.TZLabel
		updated.TZOffset = existing.TZOffset
	}

	if !updated.IsBot && existing.IsBot {
		updated.IsBot = existing.IsBot
	}

	return updated
}

func (c *Client) UpdateUser(user slack.User) {
	if user.ID == "" {
		return
	}

	c.Lock()
	if existing, ok := c.users[user.ID]; ok {
		user = mergeSlackUser(existing, user)
	}

	c.users[user.ID] = user
	c.Unlock()
}

func (c *Client) UpdatePresence(userID, presence string) {
	if userID == "" {
		return
	}

	now := time.Now().Unix()

	c.Lock()
	entry := c.presences[userID]
	entry.presence.Presence = presence
	entry.presence.Online = presence == presenceActive
	entry.verifiedAt = now

	if presence == presenceActive {
		entry.presence.LastActivity = slack.JSONTime(now)
	}

	c.presences[userID] = entry
	c.Unlock()
}

func staleFilteredPresence(entry presenceEntry, now int64) *slack.UserPresence {
	p := entry.presence
	if p.Presence != presenceActive && p.LastActivity > 0 {
		if entry.verifiedAt == 0 || now-entry.verifiedAt > int64(presenceStaleTTL.Seconds()) {
			p.LastActivity = 0
		}
	}

	return &p
}

func (c *Client) GetUserPresence(ctx context.Context, userID string) (*slack.UserPresence, error) {
	if userID == "" {
		return nil, errNoPresence
	}

	now := time.Now().Unix()

	c.RLock()
	entry, ok := c.presences[userID]
	c.RUnlock()

	if ok && entry.fetchedAt > 0 && now-entry.fetchedAt < int64(presenceCacheTTL.Seconds()) {
		p := entry.presence
		return &p, nil
	}

	if c.sc == nil {
		if ok {
			return staleFilteredPresence(entry, now), nil
		}

		return nil, errNoPresence
	}

	up, err := c.sc.GetUserPresenceContext(ctx, userID)
	if err != nil {
		if ok {
			return staleFilteredPresence(entry, now), nil
		}

		return nil, err
	}

	if up != nil {
		now = time.Now().Unix()

		c.Lock()
		c.presences[userID] = presenceEntry{
			presence:   *up,
			fetchedAt:  now,
			verifiedAt: now,
		}
		c.Unlock()
	}

	return up, nil
}

func userTimezoneLocation(user *slack.User) *time.Location {
	if user == nil {
		return nil
	}

	if user.TZ != "" {
		loc, err := time.LoadLocation(user.TZ)
		if err == nil {
			return loc
		}
	}

	if user.TZOffset != 0 || user.TZLabel != "" {
		return time.FixedZone(user.TZLabel, user.TZOffset)
	}

	return nil
}

func formatStatusExpiration(expUnix int64, userLoc *time.Location) string {
	expiry := time.Unix(expUnix, 0)
	now := time.Now().In(time.Local)
	expLocal := expiry.In(time.Local)

	timeStr := expLocal.Format("15:04")
	nowDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	expDate := time.Date(expLocal.Year(), expLocal.Month(), expLocal.Day(), 0, 0, 0, 0, time.Local)
	daysDiff := int(expDate.Sub(nowDate).Hours() / 24)

	var dateStr string

	switch daysDiff {
	case 0:
		dateStr = "Today at " + timeStr
	case 1:
		dateStr = "Tomorrow at " + timeStr
	default:
		dateStr = expLocal.Format("Mon, 02 Jan 15:04")
	}

	if userLoc != nil {
		userTimeStr := expiry.In(userLoc).Format("15:04")
		if userTimeStr != timeStr {
			dateStr += ", local time: " + userTimeStr
		}
	}

	return dateStr
}

func formatCustomStatus(user slack.User) string {
	profile := user.Profile
	if profile.StatusExpiration > 0 && time.Now().Unix() >= int64(profile.StatusExpiration) {
		return ""
	}

	emoji := strings.TrimSpace(profile.StatusEmoji)
	text := strings.TrimSpace(profile.StatusText)

	var formatted string

	switch {
	case emoji != "" && text != "":
		formatted = emoji + " " + text
	case emoji != "":
		formatted = emoji
	case text != "":
		formatted = text
	default:
		return ""
	}

	if profile.StatusExpiration > 0 {
		dateStr := formatStatusExpiration(int64(profile.StatusExpiration), userTimezoneLocation(&user))
		formatted += " (Until " + dateStr + ")"
	}

	return formatted
}

func (c *Client) GetUserCustomStatus(userID string) string {
	if userID == "" {
		return ""
	}

	c.RLock()
	user, ok := c.users[userID]
	c.RUnlock()

	if !ok {
		if fetched := c.GetUser(userID); fetched != nil {
			user = *fetched
			ok = true
		}
	}

	if !ok {
		return ""
	}

	return formatCustomStatus(user)
}

func (c *Client) GetUserLocalTime(userID string) string {
	if userID == "" {
		return ""
	}

	if loc := userTimezoneLocation(c.GetUser(userID)); loc != nil {
		return time.Now().In(loc).Format("15:04")
	}

	return ""
}

func normalizePresence(presence string) string {
	switch presence {
	case presenceActive:
		return "online"
	case presenceAway:
		return presenceAway
	default:
		return presence
	}
}

func (c *Client) GetStatuses() map[string]string {
	c.RLock()
	defer c.RUnlock()

	statuses := make(map[string]string, len(c.users))

	for id, u := range c.users {
		prefix := ""
		if entry, ok := c.presences[id]; ok {
			prefix = normalizePresence(entry.presence.Presence)
		} else if u.Presence != "" {
			prefix = normalizePresence(u.Presence)
		}

		custom := formatCustomStatus(u)

		switch {
		case prefix != "" && custom != "":
			statuses[id] = prefix + ": " + custom
		case prefix != "":
			statuses[id] = prefix
		case custom != "":
			statuses[id] = "online: " + custom
		}
	}

	return statuses
}
