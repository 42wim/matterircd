package irckit

import (
	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/slack"
	//"github.com/slack-go/slack"
)

type SlackInfo struct {
	inprogress bool
}

func (u *User) loginToSlack() error {
	cred := slack.Credentials{
		Login:  u.Credentials.Login,
		Pass:   u.Credentials.Pass,
		Team:   u.Credentials.Team,
		Server: u.Credentials.Server,
		Token:  u.Credentials.Token,
	}

	eventChan := make(chan *bridge.Event)
	br, err := slack.New(u.MmInfo.Cfg, cred, eventChan, u.addUsersToChannels)
	if err != nil {
		return err
	}

	u.br = br
	u.connected = true

	go u.handleEventChan(eventChan)

	return nil
}

func (u *User) logoutFromSlack() error {
	u.Srv.Logout(u)
	return nil
}
