package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

func (s *Slack) getSlackToken() (string, error) {
	type findTeamResponseFull struct {
		SSO    bool   `json:"sso"`
		TeamID string `json:"team_id"`
		slack.SlackResponse
	}

	type loginResponseFull struct {
		Token string `json:"token"`
		slack.SlackResponse
	}

	resp, err := http.PostForm("https://slack.com/api/auth.findTeam", url.Values{"domain": {s.credentials.Team}})
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var findTeamResponse findTeamResponseFull

	err = json.Unmarshal(body, &findTeamResponse)
	if err != nil {
		return "", err
	}

	if findTeamResponse.SSO {
		return "", errors.New("SSO teams not yet supported")
	}

	resp, err = http.PostForm("https://slack.com/api/auth.signin",
		url.Values{"team": {findTeamResponse.TeamID}, "email": {s.credentials.Login}, "password": {s.credentials.Pass}})
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var loginResponse loginResponseFull

	err = json.Unmarshal(body, &loginResponse)
	if err != nil {
		return "", err
	}

	if !loginResponse.Ok {
		return "", errors.New(loginResponse.Error)
	}

	return loginResponse.Token, nil
}

func isValidNick(s string) bool {
	/* IRC RFC ([0] - see below) mentions a limit of 9 chars for
	 * IRC nicks, but modern clients allow more than that. Let's
	 * use a "sane" big value, the triple of the spec.
	 */
	if len(s) < 1 || len(s) > 27 {
		return false
	}

	/* According to IRC RFC [0], the allowed chars to have as nick
	 * are: ( letter / special-'-' ).*( letter / digit / special ),
	 * where:
	 * letter = [a-z / A-Z]; digit = [0-9];
	 * special = [';', '[', '\', ']', '^', '_', '`', '{', '|', '}', '-']
	 *
	 * ASCII codes (decimal) for the allowed chars:
	 * letter = [65-90,97-122]; digit = [48-57]
	 * special = [59, 91-96, 123-125, 45]
	 * [0] RFC 2812 (tools.ietf.org/html/rfc2812)
	 */

	if s[0] != 59 && (s[0] < 65 || s[0] > 125) {
		return false
	}

	for i := 1; i < len(s); i++ {
		if s[i] != 45 && s[i] != 59 && (s[i] < 65 || s[i] > 125) {
			if s[i] < 48 || s[i] > 57 {
				return false
			}
		}
	}

	return true
}

func formatTS(unixts string) string {
	var targetts, targetus int64

	fmt.Sscanf(unixts, "%d.%d", &targetts, &targetus)
	ts := time.Unix(targetts, targetus*1000)

	if ts.YearDay() != time.Now().YearDay() {
		return ts.Format("2.1. 15:04:05")
	}

	return ts.Format("15:04:05")
}

// @see https://api.slack.com/docs/message-formatting#linking_to_channels_and_users
func (s *Slack) replaceMention(text string) string {
	results := regexp.MustCompile(`<@([a-zA-z0-9]+)>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.ReplaceAll(text, "<@"+r[1]+">", "@"+s.userName(r[1]))
	}

	return text
}

// @see https://api.slack.com/docs/message-formatting#linking_to_channels_and_users
func replaceChannel(text string) string {
	results := regexp.MustCompile(`<#[a-zA-Z0-9]+\|(.+?)>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.ReplaceAll(text, r[0], "#"+r[1])
	}

	return text
}

// @see https://api.slack.com/docs/message-formatting#variables
func replaceVariable(text string) string {
	results := regexp.MustCompile(`<!((?:subteam\^)?[a-zA-Z0-9]+)(?:\|@?(.+?))?>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		if r[2] != "" {
			text = strings.ReplaceAll(text, r[0], "@"+r[2])
		} else {
			text = strings.ReplaceAll(text, r[0], "@"+r[1])
		}
	}

	return text
}

// @see https://api.slack.com/docs/message-formatting#linking_to_urls
func replaceURL(text string) string {
	results := regexp.MustCompile(`<(.*?)(\|.*?)?>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.ReplaceAll(text, r[0], r[1])
	}

	return text
}

func (s *Slack) cleanupMessage(msg string) string {
	msg = s.replaceMention(msg)
	msg = replaceVariable(msg)
	msg = replaceChannel(msg)
	msg = replaceURL(msg)
	msg = html.UnescapeString(msg)

	return msg
}
