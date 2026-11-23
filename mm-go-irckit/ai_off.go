//go:build noai

package irckit

func summarize(u *User, toUser *User, args []string, service string) {
	u.MsgUser(toUser, "AI summarization is not enabled in this build.")
}
