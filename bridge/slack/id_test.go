package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
)

const testSlackChannelID = "C0BQRTTFYAK"

func assertSlackIDRoundTrip(t *testing.T, channelID, ts string) {
	t.Helper()

	id := encodeSlackID(channelID, ts)
	if !assert.NotEmpty(t, id) {
		t.FailNow()
	}

	assert.True(t, strings.HasPrefix(id, "sl"))
	assert.GreaterOrEqual(t, len(id), slackIDMinLen)
	assert.LessOrEqual(t, len(id), slackIDMaxLen)
	assert.NotEqual(t, 26, len(id), "must never match Mattermost 26-char ID length")

	for i := range len(id) {
		c := id[i]
		assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z'), "expected [0-9a-z], got %q", c)
	}

	gotCh, gotTS, ok := decodeSlackID(id)
	if !assert.True(t, ok) {
		t.FailNow()
	}

	assert.Equal(t, channelID, gotCh)
	assert.Equal(t, ts, gotTS)

	// Case-insensitive decode still restores exact channelID case.
	gotChUpper, gotTSUpper, okUpper := decodeSlackID(strings.ToUpper(id))
	if !assert.True(t, okUpper) {
		t.FailNow()
	}

	assert.Equal(t, channelID, gotChUpper)
	assert.Equal(t, ts, gotTSUpper)
}

func TestEncodeDecodeSlackID(t *testing.T) {
	tests := []struct {
		name      string
		channelID string
		ts        string
		wantLen   int
	}{
		{
			name:      "standard channel and timestamp",
			channelID: testSlackChannelID,
			ts:        "1794617246.259449",
			wantLen:   36,
		},
		{
			name:      "leading zeros in microseconds",
			channelID: "C0123456789",
			ts:        "1794617450.000300",
			wantLen:   36,
		},
		{
			name:      "dm channel",
			channelID: "D01234567",
			ts:        "1700000000.000001",
			wantLen:   36,
		},
		{
			name:      "short channel padded above 26 chars",
			channelID: "C1",
			ts:        "1700000000.123456",
			wantLen:   36,
		},
		{
			name:      "non-standard timestamp fallback",
			channelID: "G0123456789",
			ts:        "01700000000.123",
			wantLen:   49,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertSlackIDRoundTrip(t, tc.channelID, tc.ts)

			if tc.wantLen > 0 {
				assert.Len(t, encodeSlackID(tc.channelID, tc.ts), tc.wantLen)
			}
		})
	}
}

func TestEncodeDecodeSlackIDInvalid(t *testing.T) {
	assert.Empty(t, encodeSlackID("", "1794617246.259449"))
	assert.Empty(t, encodeSlackID(testSlackChannelID, ""))

	invalid := []string{
		"",
		"sl",
		"zhqrffbxupgepj1yquxaftfk4a",
		"$abc123",
		"sl" + strings.Repeat("z", 30), // 'z' is outside base32 [0-9a-v]
		"sl" + strings.Repeat("0", 30), // below minimum length
		"sl" + strings.Repeat("0", 34), // exact min length, invalid Feistel payload
	}

	for _, id := range invalid {
		ch, ts, ok := decodeSlackID(id)
		assert.False(t, ok, "expected decode failure for %q", id)
		assert.Empty(t, ch)
		assert.Empty(t, ts)
	}
}

func TestSlackIDAvalanche(t *testing.T) {
	idA := encodeSlackID(testSlackChannelID, "1794617246.259449")
	idB := encodeSlackID(testSlackChannelID, "1794617246.259450")

	assert.Len(t, idA, 36)
	assert.Len(t, idB, 36)
	assert.NotEqual(t, idA[:6], idB[:6], "adjacent timestamps should not share the prefix after sl")
}

func TestSlackTSMillis(t *testing.T) {
	assert.Equal(t, int64(1794617246259), slackTSMillis("1794617246.259449"))
	assert.Equal(t, int64(1794617450000), slackTSMillis("1794617450.000300"))
}

func TestReplySummary(t *testing.T) {
	threadRootCache, _ := lru.New[string, string](8)
	parentCache, _ := lru.New[string, slackCachedPost](8)

	s := &Slack{
		msgThreadRootCache: threadRootCache,
		msgParentCache:     parentCache,
	}

	s.cachePost(testSlackChannelID, "1794617246.259449", "1794617200.000100", &bridge.UserInfo{Nick: "alice", Ghost: true}, "Hello world this is a longer message")

	cp, ok := s.getCachedPost(context.Background(), testSlackChannelID, "1794617246.259449")
	assert.True(t, ok)
	assert.Equal(t, "1794617200.000100", cp.rootTS)
	assert.Equal(t, "alice", cp.sender.Nick)

	rcHidden := &config.RuntimeConfig{Slack: config.SlackConfig{HideReplies: true, ShortenRepliesTo: 22}}
	assert.Empty(t, formatReplySummary(rcHidden, cp))

	rcShort := &config.RuntimeConfig{Slack: config.SlackConfig{ShortenRepliesTo: 22}}
	assert.Equal(t, " (re @alice: Hello world this is a \x0f ...)", formatReplySummary(rcShort, cp))

	rcUnicode := &config.RuntimeConfig{
		Slack: config.SlackConfig{
			ShortenRepliesTo: 22,
			Formatter:        config.FormatterConfig{Unicode: true},
		},
	}
	assert.Equal(t, " (re @alice: Hello world this is a \x0f …)", formatReplySummary(rcUnicode, cp))

	rcFull := &config.RuntimeConfig{}
	assert.Equal(t, " (re @alice: Hello world this is a longer message)", formatReplySummary(rcFull, cp))
}

func TestDeletedMessageFormatting(t *testing.T) {
	threadRootCache, _ := lru.New[string, string](8)
	parentCache, _ := lru.New[string, slackCachedPost](8)
	evChan := make(chan *bridge.Event, 2)

	s := &Slack{
		eventChan:          evChan,
		msgThreadRootCache: threadRootCache,
		msgParentCache:     parentCache,
	}

	rootTS := "1794617200.000100"
	replyTS := "1794617246.259449"
	sender := &bridge.UserInfo{Nick: "alice", Me: true}
	s.cachePost(testSlackChannelID, replyTS, rootTS, sender, "Testing reply to thread")

	s.handleSlackMessageDeleted(context.Background(), &slack.MessageEvent{
		Msg: slack.Msg{
			Type:             "message",
			SubType:          subTypeMessageDeleted,
			Channel:          testSlackChannelID,
			DeletedTimestamp: replyTS,
		},
	})

	ev := <-evChan
	assert.Equal(t, "channel_message", ev.Type)

	cm, ok := ev.Data.(*bridge.ChannelMessageEvent)
	assert.True(t, ok)
	assert.Equal(t, sender, cm.Sender)
	assert.Equal(t, encodeSlackID(testSlackChannelID, replyTS), cm.MessageID)
	assert.Equal(t, encodeSlackID(testSlackChannelID, rootTS), cm.ParentID)
	assert.Equal(t, "post_deleted", cm.Event)
	assert.Equal(t, "Testing reply to thread \x1d(deleted)\x1d", cm.Text)

	_, stillCached := s.getCachedPostOnly(testSlackChannelID, replyTS)
	assert.False(t, stillCached)

	rcUnicode := &config.RuntimeConfig{Slack: config.SlackConfig{Formatter: config.FormatterConfig{Unicode: true}}}
	assert.Equal(t, "\x1d(🗑️deleted)\x1d", formatDeletedSlackMsg(rcUnicode, "", ""))
}

func TestSlackArchiveURL(t *testing.T) {
	channelID := testSlackChannelID
	rootTS := "1794557335.563729"
	replyTS := "1794635402.157409"

	assert.Equal(
		t,
		"https://haw-testing.slack.com/archives/C0BQRTTFYAK/p1794557335563729",
		formatSlackArchiveURL("haw-testing", channelID, rootTS, ""),
	)
	assert.Equal(
		t,
		"https://haw-testing.slack.com/archives/C0BQRTTFYAK/p1794557335563729",
		formatSlackArchiveURL("haw-testing", channelID, rootTS, rootTS),
	)

	replyURL := formatSlackArchiveURL("haw-testing", channelID, replyTS, rootTS)
	assert.Equal(
		t,
		"https://haw-testing.slack.com/archives/C0BQRTTFYAK/p1794635402157409?thread_ts=1794557335.563729&cid=C0BQRTTFYAK",
		replyURL,
	)

	gotCh, gotTS, ok := parseSlackArchiveURL(replyURL)
	assert.True(t, ok)
	assert.Equal(t, channelID, gotCh)
	assert.Equal(t, replyTS, gotTS)

	gotRootCh, gotRootTS, okRoot := parseSlackArchiveURL("https://haw-testing.slack.com/archives/C0BQRTTFYAK/p1794557335563729")
	assert.True(t, okRoot)
	assert.Equal(t, channelID, gotRootCh)
	assert.Equal(t, rootTS, gotRootTS)

	for _, bad := range []string{
		"",
		"not-a-url",
		"https://example.com/archives/C0BQRTTFYAK/p1794557335563729",
		"https://haw-testing.slack.com/messages/C0BQRTTFYAK/p1794557335563729",
		"https://haw-testing.slack.com/archives/C0BQRTTFYAK/p123",
	} {
		_, _, okBad := parseSlackArchiveURL(bad)
		assert.False(t, okBad, "expected parse failure for %q", bad)
	}

	s := &Slack{}
	assert.Equal(t, encodeSlackID(channelID, replyTS), s.NormalizePostID(replyURL))
	assert.Equal(t, encodeSlackID(channelID, replyTS), s.NormalizePostID(strings.ToUpper(encodeSlackID(channelID, replyTS))))
	assert.Empty(t, s.NormalizePostID("invalid"))

	events := []*bridge.Event{{
		Data: &bridge.ChannelMessageEvent{
			MessageID: encodeSlackID(channelID, rootTS),
		},
	}}
	assert.Equal(t, rootTS, firstEventRootTS(events))
	assert.Empty(t, s.PostURL(context.Background(), "invalid", events))
	assert.Empty(t, s.PostURL(context.Background(), encodeSlackID(channelID, replyTS), events))
}

func TestFormatPinnedSlackMsg(t *testing.T) {
	codeBlock := "```bash\nTesting\nTesting again\nTesting bash code block...\n```" //nolint:dupword
	itemMsg := &slack.Message{
		Msg: slack.Msg{
			Text: codeBlock,
		},
	}

	assert.Equal(
		t,
		codeBlock+" \x1d(📌pinned)\x1d",
		formatPinnedSlackMsg(nil, true, itemMsg, "", subTypePinAdded),
	)
	assert.Equal(
		t,
		codeBlock+" \x1d(unpinned)\x1d",
		formatPinnedSlackMsg(nil, false, itemMsg, "", subTypePinRemoved),
	)
	assert.Equal(
		t,
		"hello (re @alice: root) \x1d(📌unpinned)\x1d",
		formatPinnedSlackMsg(nil, true, &slack.Message{Msg: slack.Msg{Text: "hello\n"}}, " (re @alice: root)", subTypePinRemoved),
	)
	assert.Equal(
		t,
		"\x1d(📌pinned)\x1d",
		formatPinnedSlackMsg(nil, true, &slack.Message{}, "", subTypePinAdded),
	)
}
