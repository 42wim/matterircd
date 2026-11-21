package msteams

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"net/url"
	"strconv"
	"strings"

	"github.com/matterbridge/msteamsclient"
)

const (
	teamsIDPrefix    = "mt"
	teamsIDAlphabet  = "0123456789abcdefghijklmnopqrstuv"
	teamsIDPayloadSz = 13 // 8-byte uint64 msgID + 5-byte sha256(convID)[:5]
	teamsIDEncodedLn = 23 // len("mt") + ceil(13*8/5) = 2 + 21
)

var teamsIDEncoding = base32.NewEncoding(teamsIDAlphabet).WithPadding(base32.NoPadding)

func convFingerprint(convID string) [5]byte {
	sum := sha256.Sum256([]byte(convID))

	var out [5]byte
	copy(out[:], sum[:5])

	return out
}

// encodeTeamsID packs a Teams (conversationID, messageID) pair into a fixed
// 23-character lowercase identifier prefixed with "mt" (mt[0-9a-v]{21}).
// It is unambiguous against Mattermost (26 chars), Matrix ($...), and Slack (sl...).
func encodeTeamsID(convID, msgID string) string {
	if convID == "" || msgID == "" {
		return msgID
	}

	num, err := strconv.ParseUint(msgID, 10, 64)
	if err != nil {
		return msgID
	}

	var buf [teamsIDPayloadSz]byte
	binary.BigEndian.PutUint64(buf[:8], num)

	fp := convFingerprint(convID)
	copy(buf[8:], fp[:])

	return teamsIDPrefix + teamsIDEncoding.EncodeToString(buf[:])
}

// decodeTeamsID decodes an mt[0-9a-v]{21} identifier into its numeric Teams
// message ID and 40-bit conversation fingerprint.
func decodeTeamsID(encoded string) (string, [5]byte, bool) {
	var zero [5]byte

	s := strings.ToLower(strings.TrimSpace(encoded))
	if len(s) != teamsIDEncodedLn || !strings.HasPrefix(s, teamsIDPrefix) {
		return "", zero, false
	}

	raw, err := teamsIDEncoding.DecodeString(s[len(teamsIDPrefix):])
	if err != nil || len(raw) != teamsIDPayloadSz {
		return "", zero, false
	}

	num := binary.BigEndian.Uint64(raw[:8])

	var fp [5]byte
	copy(fp[:], raw[8:])

	return strconv.FormatUint(num, 10), fp, true
}

// parseTeamsMessageURL extracts (conversationID, messageID) from a Teams web
// deep link such as:
//   - https://teams.microsoft.com/l/message/<convID>/<msgID>?...
//   - https://teams.live.com/l/message/<convID>/<msgID>?...
//   - https://teams.cloud.microsoft/l/message/<convID>/<msgID>?...
func parseTeamsMessageURL(raw string) (string, string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", false
	}

	host := strings.ToLower(u.Hostname())
	if host != "teams.microsoft.com" && host != "teams.live.com" && host != "teams.cloud.microsoft" {
		return "", "", false
	}

	path := strings.TrimPrefix(u.EscapedPath(), "/")
	parts := strings.Split(path, "/")

	if len(parts) < 4 || parts[0] != "l" || parts[1] != "message" {
		return "", "", false
	}

	convID, err := url.PathUnescape(parts[2])
	if err != nil || convID == "" {
		return "", "", false
	}

	msgID := parts[3]

	_, err = strconv.ParseUint(msgID, 10, 64)
	if err != nil {
		return "", "", false
	}

	return convID, msgID, true
}

func teamsMessageURL(acctType msteamsclient.AccountType, convID, msgID string) string {
	if convID == "" || msgID == "" {
		return ""
	}

	host := "teams.microsoft.com"
	if acctType == msteamsclient.AccountPersonal {
		host = "teams.live.com"
	}

	return "https://" + host + "/l/message/" + url.PathEscape(convID) + "/" + url.PathEscape(msgID)
}
