package slackclient

import (
	"strings"

	"github.com/slack-go/slack"
)

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
	me, _ := c.sc.GetUserInfo(c.info.User.ID)
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
	case "away":
		return c.sc.SetUserPresence("away")
	}

	return nil
}

func (c *Client) UserID() string {
	return c.info.User.ID
}

func (c *Client) CurrentUserName() string {
	return c.info.User.Name
}

func (c *Client) TeamID() string {
	return c.info.Team.ID
}

func (c *Client) TeamName() string {
	return c.info.Team.Name
}

func (c *Client) TeamDomain() string {
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
