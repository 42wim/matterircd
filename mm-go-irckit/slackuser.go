package irckit

import (
	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/slack"
)

func (u *User) loginToSlack() error {
	eventChan := make(chan *bridge.Event)
	br, err := slack.New(u.v, u.Credentials, eventChan, u.addUsersToChannels)
	if err != nil {
		return err
	}

	u.br = br

	go u.handleEventChan(eventChan)

	return nil
}

func (u *User) logoutFromSlack() error {
	u.Srv.Logout(u)
	return nil
}
