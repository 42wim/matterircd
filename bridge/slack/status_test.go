package slack

import (
	"context"
	"testing"
	"time"

	"github.com/matterbridge/slackclient"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
)

func TestIsOnline(t *testing.T) {
	s := &Slack{}

	assert.True(t, s.IsOnline("online"))
	assert.True(t, s.IsOnline("active"))
	assert.True(t, s.IsOnline("online (Last activity just now, local time: 14:30)"))
	assert.True(t, s.IsOnline("active (Last activity just now)"))
	assert.False(t, s.IsOnline("online (Last activity just now): In a meeting"))
	assert.False(t, s.IsOnline("online: In a meeting"))
	assert.False(t, s.IsOnline("away"))
	assert.False(t, s.IsOnline("away (Last online 1 hr. ago)"))
	assert.False(t, s.IsOnline(""))
}

func TestNormalizeSlackPresence(t *testing.T) {
	assert.Equal(t, "online", normalizeSlackPresence("active"))
	assert.Equal(t, "away", normalizeSlackPresence("away"))
	assert.Empty(t, normalizeSlackPresence(""))
}

func TestFormatSlackUserStatus(t *testing.T) {
	now := time.Now().Unix()

	assert.Equal(t, "online", formatSlackUserStatus("online", "", "", 0))
	assert.Equal(t, "online (Last activity just now, local time: 14:30)",
		formatSlackUserStatus("online", "14:30", "", now-5))
	assert.Equal(t, "online (Last activity just now, local time: 14:30): :calendar: In a meeting",
		formatSlackUserStatus("online", "14:30", ":calendar: In a meeting", now-5))

	// Away users omit Last online if activity was < 20 minutes ago.
	assert.Equal(t, "away (local time: 09:15)",
		formatSlackUserStatus("away", "09:15", "", now-10*60))

	// Away users include Last online if activity was >= 20 minutes ago.
	assert.Equal(t, "away (Last online 1 hr. ago, local time: 09:15): Lunch",
		formatSlackUserStatus("away", "09:15", "Lunch", now-3600))
}

func TestSlackClientStatusHelpers(t *testing.T) {
	sc := slackclient.New(slackclient.Credentials{})
	future := int(time.Now().Add(time.Hour).Unix())
	past := int(time.Now().Add(-time.Hour).Unix())

	sc.UpdateUser(slack.User{
		ID: "U1",
		TZ: "UTC",
		Profile: slack.UserProfile{
			StatusEmoji:      ":coffee:",
			StatusText:       "Coffee break",
			StatusExpiration: future,
		},
	})
	sc.UpdateUser(slack.User{
		ID:       "U2",
		TZLabel:  "AEST",
		TZOffset: 10 * 3600,
		Profile: slack.UserProfile{
			StatusEmoji:      ":palm_tree:",
			StatusText:       "Expired",
			StatusExpiration: past,
		},
	})
	sc.UpdatePresence("U1", "active")
	sc.UpdatePresence("U2", "away")

	assert.Equal(t, ":coffee: Coffee break", sc.GetUserCustomStatus("U1"))
	assert.Empty(t, sc.GetUserCustomStatus("U2"))

	assert.Equal(t, time.Now().UTC().Format("15:04"), sc.GetUserLocalTime("U1"))
	assert.Equal(t, time.Now().In(time.FixedZone("AEST", 10*3600)).Format("15:04"), sc.GetUserLocalTime("U2"))

	statuses := sc.GetStatuses()
	assert.Equal(t, "online: :coffee: Coffee break", statuses["U1"])
	assert.Equal(t, "away", statuses["U2"])

	up, err := sc.GetUserPresence(context.Background(), "U1")
	if err != nil {
		t.Fatalf("GetUserPresence failed: %v", err)
	}

	if up == nil {
		t.Fatal("expected non-nil UserPresence")
	}

	assert.Equal(t, "active", up.Presence)
	assert.True(t, up.Online)
	assert.NotZero(t, up.LastActivity)
}

func TestBuildSlackUserInfo(t *testing.T) {
	u := &slack.User{
		ID:       "U123",
		RealName: "Jane Doe",
		IsBot:    true,
		Profile: slack.UserProfile{
			DisplayName: "jane",
			Email:       "jane@example.com",
			Title:       "Staff Engineer",
			Image192:    "https://example.com/192.png",
			Image512:    "https://example.com/512.png",
		},
	}

	info := buildSlackUserInfo(u, "jane", "T1", false)
	assert.Equal(t, "U123", info.User)
	assert.Equal(t, "bot", info.Roles)
	assert.Equal(t, "jane@example.com", info.Email)
	assert.Equal(t, "Staff Engineer", info.Position)
	assert.Equal(t, "https://example.com/512.png", info.Photo)
}
