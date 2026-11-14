package irckit

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

type ParseTest struct {
	Desc    string
	Value   string
	Results []string
	IsGood  bool
}

var loginTests = []ParseTest{
	{
		Desc:    "base simple case",
		Value:   "login user password",
		Results: []string{"login", "user", "password"}, //nolint:goconst
		IsGood:  true,
	},
	{
		Desc:    "Simple with server",
		Value:   "login server user password",
		Results: []string{"login", "server", "user", "password"}, //nolint:goconst
		IsGood:  true,
	},
	{
		Desc:    "Simple with server and team",
		Value:   "login server team user password",
		Results: []string{"login", "server", "team", "user", "password"}, //nolint:goconst
		IsGood:  true,
	},
	{
		Desc:    "Simple Long Password Login Single Quote",
		Value:   "login user 'long password'",
		Results: []string{"login", "user", "long password"}, //nolint:goconst
		IsGood:  true,
	},
	{
		Desc:    "Long Password Login (server) Single Quote",
		Value:   "login server user 'long password'",
		Results: []string{"login", "server", "user", "long password"},
		IsGood:  true,
	},
	{
		Desc:    "Long Password Login (server & team) Single Quote",
		Value:   "login server team user 'long password'",
		Results: []string{"login", "server", "team", "user", "long password"},
		IsGood:  true,
	},
	{
		Desc:    "Simple Long Password Login Double Quote",
		Value:   "login user \"long password\"",
		Results: []string{"login", "user", "long password"},
		IsGood:  true,
	},
	{
		Desc:    "Long Password Login (server) Double Quote",
		Value:   "login server user \"long password\"",
		Results: []string{"login", "server", "user", "long password"},
		IsGood:  true,
	},
	{
		Desc:    "Long Password Login (server & team) Double Quote",
		Value:   "login server team user \"long password\"",
		Results: []string{"login", "server", "team", "user", "long password"},
		IsGood:  true,
	},
	{
		Desc:    "Test Last Space",
		Value:   "login user \"long password\" ",
		Results: []string{"login", "user", "long password"},
		IsGood:  true,
	},
	{
		Desc:    "Test Escape",
		Value:   "login user \"\\&long password\"",
		Results: []string{"login", "user", "&long password"},
		IsGood:  true,
	},
	{
		Desc:    "Test Escape Quote",
		Value:   "login user \"\\\"long password\"",
		Results: []string{"login", "user", "\"long password"},
		IsGood:  true,
	},
}

func TestParseCommandString(t *testing.T) {
	fmt.Println("--------------------")

	for _, tc := range loginTests {
		results, err := parseCommandString(tc.Value)
		if err != nil && tc.IsGood {
			t.Fatal(tc.Desc + " should be an error.")
		}

		fmt.Println(tc.Desc)

		for i, r := range tc.Results {
			fmt.Printf("Testing [%s] == [%s]\n", results[i], r)
			assert.Equal(t, results[i], r)
		}
	}
}

func TestIsSlackToken(t *testing.T) {
	//nolint:gosec // test fixtures for Slack token format matching
	tests := []struct {
		pass string
		want bool
	}{
		{pass: "xoxp-1234567890-1234567890-abcdef", want: true},
		{pass: "xoxc-1234567890-1234567890-abcdef|d=xoxd-1234567890abcdef;", want: true},
		{pass: "xoxc-1234567890-1234567890-abcdef|d=xoxd-1234567890abcdef", want: false},
		{pass: "xoxp-", want: false},
		{pass: "xoxp-short", want: false},
		{pass: "xoxc-1234567890-1234567890-abcdef", want: false},
		{pass: "xoxc-short|d=xoxd-1234567890abcdef;", want: false},
		{pass: "xoxc-1234567890-1234567890-abcdef|d=xoxd-short;", want: false},
		{pass: "xoxc-1234567890-1234567890-abcdef|d=other", want: false},
		{pass: "some-mattermost-password", want: false},
		{pass: "", want: false},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, isSlackToken(tc.pass), tc.pass)
	}
}

func TestSlackIDPatterns(t *testing.T) {
	slackID := "slfjm1dbhvpijvf4el03a39vq5p19benbsus"

	assert.True(t, isBareThreadID("@@"+slackID))
	assert.False(t, isBareThreadID("@@"+slackID+" reply text"))
	assert.False(t, isBareThreadID("@@slshort"))
	assert.False(t, isBareThreadID("@@sle4kb6g2mq3bs2eslbs949fmvvk"))

	assert.Equal(t, "@@"+slackID+" ", threadPrefix("@@"+slackID+" hello"))
	assert.True(t, modifyRegExp.MatchString("s/"+slackID+"/updated"))

	threadMatches := parseThreadIDRegExp.FindStringSubmatch("@@" + slackID + " hello\nworld")
	assert.Len(t, threadMatches, 3)
	assert.Equal(t, slackID, threadMatches[1])
	assert.Equal(t, "hello\nworld", threadMatches[2])

	reactMatches := parseReactionToMsgRegExp.FindStringSubmatch("@@" + slackID + " +:thumbsup:")
	assert.Len(t, reactMatches, 4)
	assert.Equal(t, slackID, reactMatches[1])
	assert.Equal(t, "+", reactMatches[2])
	assert.Equal(t, "thumbsup", reactMatches[3])

	modMatches := parseModifyMsgRegExp.FindStringSubmatch("s/" + slackID + "/new text")
	assert.Len(t, modMatches, 3)
	assert.Equal(t, "/"+slackID+"/", modMatches[1])
	assert.Equal(t, "new text", modMatches[2])
}
