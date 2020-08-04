package slack

import (
	"fmt"
	"strings"

	"github.com/42wim/matterircd/bridge"
	"github.com/slack-go/slack"
)

type Slack struct {
	sc    *slack.Client
	sinfo *slack.Info
}

func New(sc *slack.Client, sinfo *slack.Info) bridge.Bridger {
	return &Slack{
		sc: sc,
	}
}

func (s *Slack) Invite(channelID, username string) error {
	_, err := s.sc.InviteUsersToConversation(strings.ToUpper(channelID), username)
	return err
}

func (s *Slack) Join(channelName string) (string, string, error) {
	//TODO: handle warnings
	mychan, _, _, err := s.sc.JoinConversation(channelName)
	if err != nil {
		return "", "", fmt.Errorf("Cannot join channel (+i): %s", err)
	}

	return mychan.ID, mychan.Topic.Value, nil
}

func (s *Slack) List() (map[string]string, error) {
	channelinfo := make(map[string]string)

	params := slack.GetConversationsParameters{
		Cursor:          "",
		ExcludeArchived: "true",
		Limit:           100,
		Types:           []string{"public_channel", "private_channel", "mpim"},
	}

OUTER:
	for {
		conversations, nextCursor, _ := s.sc.GetConversations(&params)
		params.Cursor = nextCursor

		for _, channel := range conversations {
			channelinfo["#"+channel.Name] = strings.Replace(channel.Topic.Value, "\n", " | ", -1)
			if nextCursor == "" {
				break OUTER
			}
		}

	}

	return channelinfo, nil
}

func (s *Slack) Part(channelID string) error {
	_, err := s.sc.LeaveConversation(strings.ToUpper(channelID))
	return err
}

func (s *Slack) UpdateChannels() error {
	return nil
}

func (s *Slack) Logout() error {
	return nil
}

func (s *Slack) MsgUser(username, text string) error {
	_, _, dchannel, err := s.sc.OpenIMChannel(username)
	if err != nil {
		return err
	}

	np := slack.NewPostMessageParameters()
	np.AsUser = true
	//np.Username = u.User
	var attachments []slack.Attachment
	attachments = append(attachments, slack.Attachment{CallbackID: "matterircd_" + s.sinfo.User.ID})

	var opts []slack.MsgOption
	opts = append(opts, slack.MsgOptionAttachments(attachments...))
	opts = append(opts, slack.MsgOptionPostMessageParameters(np))
	opts = append(opts, slack.MsgOptionText(text, false))

	_, _, err = s.sc.PostMessage(dchannel, opts...)
	if err != nil {
		return err
	}
	return nil
}

func (s *Slack) MsgChannel(channelID, text string) error {
	var attachments []slack.Attachment

	np := slack.NewPostMessageParameters()
	np.AsUser = true
	np.LinkNames = 1
	//np.Username = u.User
	attachments = append(attachments, slack.Attachment{CallbackID: "matterircd_" + s.sinfo.User.ID})

	var opts []slack.MsgOption
	opts = append(opts, slack.MsgOptionAttachments(attachments...))
	opts = append(opts, slack.MsgOptionPostMessageParameters(np))
	opts = append(opts, slack.MsgOptionText(text, false))

	_, _, err := s.sc.PostMessage(strings.ToUpper(channelID), opts...)
	if err != nil {
		return err
	}

	return nil
}

func (s *Slack) Topic(channelID string) string {
	/*
		_, err := s.sc.SetTopicOfConversation(strings.ToUpper(channelID), text)
		return err
	*/
	return ""
}

func (s *Slack) SetTopic(channelID, text string) error {
	_, err := s.sc.SetTopicOfConversation(strings.ToUpper(channelID), text)
	return err
}

func (s *Slack) StatusUser(name string) (string, error) {
	return "", nil
}

func (s *Slack) StatusUsers() (map[string]string, error) {
	return make(map[string]string), nil
}

func (s *Slack) Protocol() string {
	return "slack"
}

func (s *Slack) Kick(channelID, username string) error {
	return s.sc.KickUserFromConversation(strings.ToUpper(channelID), username)
}

func (s *Slack) SetStatus(status string) error {
	switch status {
	case "online":
		return s.sc.SetUserPresence("auto")
	case "away":
		return s.sc.SetUserPresence("away")
	}

	return nil
}

func (s *Slack) Nick(name string) error {
	return nil
}

func (s *Slack) GetChannelName(channelID string) string {
	var name string

	info, err := s.sc.GetConversationInfo(channelID, false)
	if err != nil {
		name = channelID
	} else {
		name = "#" + info.Name
	}

	return name
}

func (s *Slack) GetChannelUsers(channelID string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (s *Slack) GetUsers() []*bridge.UserInfo {
	return nil
}

func (s *Slack) GetChannels() []*bridge.ChannelInfo {
	return nil
}

func (s *Slack) GetUser(userID string) *bridge.UserInfo {
	return nil
}

func (s *Slack) GetMe() *bridge.UserInfo {
	return nil
}

func (s *Slack) GetUserByUsername(username string) *bridge.UserInfo {
	return nil
}
