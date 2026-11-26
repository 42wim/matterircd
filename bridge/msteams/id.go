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
	teamsIDAlphabet    = "0123456789abcdefghijklmnopqrstuv"
	teamsIDPayloadSz   = 10 // 6-byte uint48 msgID (ms, valid through year 10889) + 4-byte sha256(convID)[:4]
	teamsIDEncodedLn   = 16 // (10*8/5) = 16
	teamsIDFeistelHead = 4
	maxUint48          = (uint64(1) << 48) - 1
)

var teamsIDEncoding = base32.NewEncoding(teamsIDAlphabet).WithPadding(base32.NoPadding)

func xorMask(dst, mask []byte) {
	for i := range dst {
		dst[i] ^= mask[i]
	}
}

func feistelMask(round byte, in []byte, n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	buf := make([]byte, 2+len(in))
	buf[0] = round
	copy(buf[2:], in)

	for ctr := byte(0); len(out) < n; ctr++ {
		buf[1] = ctr
		sum := sha256.Sum256(buf)
		out = append(out, sum[:]...)
	}

	return out[:n]
}

// mixTeamsPayload applies (or reverses) a deterministic 4-round Feistel mix so
// encoded IDs appear uniform rather than sharing timestamp prefixes.
func mixTeamsPayload(data []byte, forward bool) {
	if len(data) <= teamsIDFeistelHead {
		return
	}

	h := data[:teamsIDFeistelHead]
	b := data[teamsIDFeistelHead:]

	if forward {
		xorMask(b, feistelMask(1, h, len(b)))
		xorMask(h, feistelMask(2, b, teamsIDFeistelHead))
		xorMask(b, feistelMask(3, h, len(b)))
		xorMask(h, feistelMask(4, b, teamsIDFeistelHead))

		return
	}

	xorMask(h, feistelMask(4, b, teamsIDFeistelHead))
	xorMask(b, feistelMask(3, h, len(b)))
	xorMask(h, feistelMask(2, b, teamsIDFeistelHead))
	xorMask(b, feistelMask(1, h, len(b)))
}

func convFingerprint(convID string) [4]byte {
	sum := sha256.Sum256([]byte(convID))

	var out [4]byte
	copy(out[:], sum[:4])

	return out
}

// encodeTeamsID packs a Teams (conversationID, messageID) pair into a fixed
// 16-character lowercase identifier ([0-9a-v]{16}).
// It is unambiguous against Google Chat (11 chars), Rocket.Chat (17 chars),
// Mattermost (26 chars), Slack (34-80 chars), and Matrix ($...).
func encodeTeamsID(convID, msgID string) string {
	if convID == "" || msgID == "" {
		return msgID
	}

	num, err := strconv.ParseUint(msgID, 10, 64)
	if err != nil || num > maxUint48 {
		return msgID
	}

	var be8 [8]byte
	binary.BigEndian.PutUint64(be8[:], num)

	var buf [teamsIDPayloadSz]byte
	copy(buf[:6], be8[2:])

	fp := convFingerprint(convID)
	copy(buf[6:], fp[:])

	mixTeamsPayload(buf[:], true)

	return teamsIDEncoding.EncodeToString(buf[:])
}

// decodeTeamsID decodes a [0-9a-v]{16} identifier into its numeric Teams
// message ID and 32-bit conversation fingerprint.
func decodeTeamsID(encoded string) (string, [4]byte, bool) {
	var zero [4]byte

	s := strings.ToLower(strings.TrimSpace(encoded))
	if len(s) != teamsIDEncodedLn {
		return "", zero, false
	}

	raw, err := teamsIDEncoding.DecodeString(s)
	if err != nil || len(raw) != teamsIDPayloadSz {
		return "", zero, false
	}

	mixTeamsPayload(raw, false)

	var be8 [8]byte
	copy(be8[2:], raw[:6])

	num := binary.BigEndian.Uint64(be8[:])

	var fp [4]byte
	copy(fp[:], raw[6:])

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
