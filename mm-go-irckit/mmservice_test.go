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
		Results: []string{"login", "user", "password"},
		IsGood:  true,
	},
	{
		Desc:    "Simple with server",
		Value:   "login server user password",
		Results: []string{"login", "server", "user", "password"},
		IsGood:  true,
	},
	{
		Desc:    "Simple with server and team",
		Value:   "login server team user password",
		Results: []string{"login", "server", "team", "user", "password"},
		IsGood:  true,
	},
	{
		Desc:    "Simple Long Password Login Single Quote",
		Value:   "login user 'long password'",
		Results: []string{"login", "user", "long password"},
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
