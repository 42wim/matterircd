package slack

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

const (
	slackIDPrefix      = "sl"
	slackIDAlphabet    = "0123456789abcdefghijklmnopqrstuv"
	slackIDMinPayload  = 21 // ceil(21*8/5) = 34 base32 chars -> 36 total chars (uniform for <=11-char channels)
	slackIDMinLen      = 36
	slackIDMaxLen      = 82
	slackIDFeistelHead = 4

	slackIDVerCompact = 0x01
	slackIDVerRaw     = 0x02

	microsPerSec = 1_000_000
)

var slackIDEncoding = base32.NewEncoding(slackIDAlphabet).WithPadding(base32.NoPadding)

func parseStandardSlackTS(ts string) (uint64, bool) {
	secStr, fracStr, found := strings.Cut(ts, ".")
	if !found || len(fracStr) != 6 {
		return 0, false
	}

	sec, err := strconv.ParseUint(secStr, 10, 64)
	if err != nil || strconv.FormatUint(sec, 10) != secStr {
		return 0, false
	}

	usec, err := strconv.ParseUint(fracStr, 10, 64)
	if err != nil || usec >= microsPerSec {
		return 0, false
	}

	if sec > (math.MaxUint64-(microsPerSec-1))/microsPerSec {
		return 0, false
	}

	return sec*microsPerSec + usec, true
}

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

// mixSlackPayload applies (or reverses) a deterministic 4-round Feistel mix so
// encoded IDs appear uniform rather than sharing fixed headers or padding.
func mixSlackPayload(data []byte, forward bool) {
	if len(data) <= slackIDFeistelHead {
		return
	}

	h := data[:slackIDFeistelHead]
	b := data[slackIDFeistelHead:]

	if forward {
		xorMask(b, feistelMask(1, h, len(b)))
		xorMask(h, feistelMask(2, b, slackIDFeistelHead))
		xorMask(b, feistelMask(3, h, len(b)))
		xorMask(h, feistelMask(4, b, slackIDFeistelHead))

		return
	}

	xorMask(h, feistelMask(4, b, slackIDFeistelHead))
	xorMask(b, feistelMask(3, h, len(b)))
	xorMask(h, feistelMask(2, b, slackIDFeistelHead))
	xorMask(b, feistelMask(1, h, len(b)))
}

// encodeSlackID reversibly encodes a Slack (channelID, ts) pair into a
// lowercase [0-9a-z] identifier prefixed with "sl".
func encodeSlackID(channelID, ts string) string {
	if channelID == "" || ts == "" || len(channelID) > 255 {
		return ""
	}

	var payload []byte

	if totalUsec, ok := parseStandardSlackTS(ts); ok {
		payloadLen := max(10+len(channelID), slackIDMinPayload)
		payload = make([]byte, payloadLen)
		payload[0] = slackIDVerCompact
		binary.BigEndian.PutUint64(payload[1:9], totalUsec)
		payload[9] = byte(len(channelID)) //nolint:gosec // length checked <= 255
		copy(payload[10:], channelID)
	} else {
		if len(ts) > 255 {
			return ""
		}

		payloadLen := max(3+len(channelID)+len(ts), slackIDMinPayload)
		payload = make([]byte, payloadLen)
		payload[0] = slackIDVerRaw
		payload[1] = byte(len(channelID)) //nolint:gosec // length checked <= 255
		payload[2] = byte(len(ts))        //nolint:gosec // length checked <= 255
		copy(payload[3:], channelID)
		copy(payload[3+len(channelID):], ts)
	}

	mixSlackPayload(payload, true)

	encoded := slackIDPrefix + slackIDEncoding.EncodeToString(payload)
	if len(encoded) > slackIDMaxLen {
		return ""
	}

	return encoded
}

func isAllZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}

	return true
}

func decodeCompactSlackID(raw []byte) (string, string, bool) {
	chLen := int(raw[9])
	if chLen == 0 || 10+chLen > len(raw) || !isAllZero(raw[10+chLen:]) {
		return "", "", false
	}

	totalUsec := binary.BigEndian.Uint64(raw[1:9])
	channelID := string(raw[10 : 10+chLen])
	ts := fmt.Sprintf("%d.%06d", totalUsec/microsPerSec, totalUsec%microsPerSec)

	return channelID, ts, true
}

func decodeRawSlackID(raw []byte) (string, string, bool) {
	chLen := int(raw[1])
	tsLen := int(raw[2])

	if chLen == 0 || tsLen == 0 || 3+chLen+tsLen > len(raw) || !isAllZero(raw[3+chLen+tsLen:]) {
		return "", "", false
	}

	channelID := string(raw[3 : 3+chLen])
	ts := string(raw[3+chLen : 3+chLen+tsLen])

	return channelID, ts, true
}

// decodeSlackID reverses an identifier produced by encodeSlackID back into
// its Slack channelID and message timestamp.
func decodeSlackID(postID string) (string, string, bool) {
	postID = strings.ToLower(postID)
	if !strings.HasPrefix(postID, slackIDPrefix) || len(postID) < slackIDMinLen || len(postID) > slackIDMaxLen {
		return "", "", false
	}

	raw, err := slackIDEncoding.DecodeString(postID[len(slackIDPrefix):])
	if err != nil || len(raw) < slackIDMinPayload {
		return "", "", false
	}

	mixSlackPayload(raw, false)

	switch raw[0] {
	case slackIDVerCompact:
		return decodeCompactSlackID(raw)
	case slackIDVerRaw:
		return decodeRawSlackID(raw)
	default:
		return "", "", false
	}
}

func formatSlackArchiveURL(teamDomain, channelID, ts, rootTS string) string {
	if teamDomain == "" || channelID == "" || ts == "" {
		return ""
	}

	ch := strings.ToUpper(channelID)
	pTS := strings.ReplaceAll(ts, ".", "")
	archiveURL := fmt.Sprintf("https://%s.slack.com/archives/%s/p%s", teamDomain, ch, pTS)

	if rootTS != "" && rootTS != ts {
		return archiveURL + "?thread_ts=" + rootTS + "&cid=" + ch
	}

	return archiveURL
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

func parseSlackArchiveURL(rawURL string) (string, string, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" && u.Scheme != "http" {
		return "", "", false
	}

	if !strings.HasSuffix(strings.ToLower(u.Hostname()), ".slack.com") {
		return "", "", false
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "archives" || parts[1] == "" || !strings.HasPrefix(parts[2], "p") {
		return "", "", false
	}

	rawTS := parts[2][1:]
	if len(rawTS) <= 6 || !isASCIIDigits(rawTS) {
		return "", "", false
	}

	ts := rawTS[:len(rawTS)-6] + "." + rawTS[len(rawTS)-6:]

	return parts[1], ts, true
}

func slackTSMillis(ts string) int64 {
	if totalUsec, ok := parseStandardSlackTS(ts); ok {
		return int64(totalUsec / 1000) //nolint:gosec // bounded timestamp value
	}

	var sec, usec int64

	_, _ = fmt.Sscanf(ts, "%d.%d", &sec, &usec)

	return sec*1000 + usec/1000
}
