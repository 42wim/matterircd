package googlechatclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	searchBatchExecuteRPCID = "SBNmJb"
)

func (c *Client) searchMessagesServer(ctx context.Context, query string, limit int) ([]*ChatMessage, error) {
	err := c.ensureBoqSession(ctx)
	if err != nil {
		return nil, err
	}

	fReq, err := buildSearchBatchExecutePayload(query)
	if err != nil {
		return nil, err
	}

	respBytes, err := c.doBatchExecute(ctx, searchBatchExecuteRPCID, "/app/search", fReq)
	if err != nil {
		return nil, err
	}

	payloadJSON, err := extractBatchExecutePayload(respBytes, searchBatchExecuteRPCID)
	if err != nil {
		return nil, err
	}

	return c.parseSearchResponse(payloadJSON, limit)
}

func (c *Client) ensureBoqSession(ctx context.Context) error {
	c.authMu.RLock()
	hasAT := c.boqAT != ""
	c.authMu.RUnlock()

	if hasAT {
		return nil
	}

	err := c.refreshXSRFToken(ctx)
	if err != nil {
		return err
	}

	c.authMu.RLock()
	hasAT = c.boqAT != ""
	c.authMu.RUnlock()

	if !hasAT {
		return errors.New("googlechatclient: missing SNlM0e token in WIZ_global_data")
	}

	return nil
}

func buildSearchBatchExecutePayload(query string) (string, error) {
	sessionUUID := newSearchUUID()

	inner := []any{
		nil,
		nil,
		nil,
		query,
		nil,
		sessionUUID,
		[]any{
			[]any{},
			nil,
			nil,
			nil,
			sessionUUID,
			nil,
			0,
			[]any{
				[]any{[]any{[]any{[]any{[]any{1}}}}},
				[]any{[]any{[]any{1}}},
			},
		},
		nil,
		[]any{2},
		[]any{143},
		0,
		nil,
		1,
	}

	innerBytes, err := json.Marshal(inner)
	if err != nil {
		return "", err
	}

	outer := [][][]any{{
		{searchBatchExecuteRPCID, string(innerBytes), nil, "generic"},
	}}

	outerBytes, err := json.Marshal(outer)
	if err != nil {
		return "", err
	}

	return string(outerBytes), nil
}

func (c *Client) doBatchExecute(ctx context.Context, rpcID, sourcePath, fReq string) ([]byte, error) {
	const opName = "/_/DynamiteWebUi/data/batchexecute"

	for attempt := 0; ; attempt++ {
		respBytes, status, header, err := c.doBatchExecuteOnce(ctx, rpcID, sourcePath, fReq)
		if err == nil && status > 0 && status < http.StatusBadRequest {
			return respBytes, nil
		}

		var resp *http.Response
		if status > 0 {
			resp = &http.Response{
				StatusCode: status,
				Header:     header,
			}
		}

		shouldRetry, retryErr := c.HandleRetry(ctx, opName, err, attempt, maxAPIRetries, resp)
		if retryErr != nil {
			return nil, retryErr
		}

		if shouldRetry {
			continue
		}

		if err != nil {
			return nil, err
		}

		if status > 0 {
			return nil, fmt.Errorf("googlechatclient: POST %s returned HTTP %d: %s", opName, status, truncateBody(respBytes))
		}

		return nil, fmt.Errorf("googlechatclient: POST %s failed with nil response", opName)
	}
}

func (c *Client) doBatchExecuteOnce(ctx context.Context, rpcID, sourcePath, fReq string) ([]byte, int, http.Header, error) {
	c.authMu.RLock()
	boqAT := c.boqAT
	boqSID := c.boqSID
	boqBL := c.boqBL
	c.authMu.RUnlock()

	params := url.Values{}
	params.Set("rpcids", rpcID)
	params.Set("source-path", sourcePath)
	params.Set("hl", "en")
	params.Set("soc-app", "1")
	params.Set("soc-platform", "1")
	params.Set("soc-device", "1")
	params.Set("_reqid", strconv.FormatInt(c.apiReqSeq.Add(1)*100000+1234, 10))
	params.Set("rt", "c")

	if boqSID != "" {
		params.Set("f.sid", boqSID)
	}

	if boqBL != "" {
		params.Set("bl", boqBL)
	}

	rawURL := c.chatBaseURL() + "/_/DynamiteWebUi/data/batchexecute?" + params.Encode()

	form := url.Values{}
	form.Set("f.req", fReq)

	if boqAT != "" {
		form.Set("at", boqAT)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	c.applyCookieAndAuthUserHeaders(req)
	req.Header.Set("Origin", chatOriginURL)
	req.Header.Set("Referer", chatOriginURL+"/")
	req.Header.Set("User-Agent", defaultBrowserUA)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}

	defer resp.Body.Close()

	c.absorbResponseCookies(resp.Cookies())

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize))
	if readErr != nil {
		return nil, resp.StatusCode, resp.Header, readErr
	}

	return body, resp.StatusCode, resp.Header, nil
}

func extractBatchExecutePayload(raw []byte, rpcID string) (string, error) {
	// Boq rt=c length prefixes count UTF-16 code units rather than UTF-8 bytes,
	// so reading by byte count truncates responses containing non-ASCII text.
	// Each JSON envelope sits on its own line starting with '['.
	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			continue
		}

		if payload, ok := findWRBPayload(trimmed, rpcID); ok {
			return payload, nil
		}
	}

	return "", fmt.Errorf("googlechatclient: missing %s payload in batchexecute response: %s", rpcID, truncateBody(raw))
}

func findWRBPayload(chunk []byte, rpcID string) (string, bool) {
	var rows [][]json.RawMessage

	err := json.Unmarshal(bytes.TrimSpace(chunk), &rows)
	if err != nil {
		return "", false
	}

	for _, row := range rows {
		if len(row) < 3 {
			continue
		}

		if parseJSONString(row[0]) != "wrb.fr" || parseJSONString(row[1]) != rpcID {
			continue
		}

		payload := parseJSONString(row[2])
		if payload != "" {
			return payload, true
		}
	}

	return "", false
}

func (c *Client) parseSearchResponse(payloadJSON string, limit int) ([]*ChatMessage, error) {
	var root []json.RawMessage

	err := json.Unmarshal([]byte(payloadJSON), &root)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode search response: %w", err)
	}

	if len(root) <= 9 {
		return nil, nil
	}

	var hits [][]json.RawMessage

	err = json.Unmarshal(root[9], &hits)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode search hits: %w", err)
	}

	out := make([]*ChatMessage, 0, len(hits))

	for _, hit := range hits {
		if len(hit) == 0 {
			continue
		}

		msg := c.parseSearchHitMessage(hit[0])
		if msg == nil {
			continue
		}

		c.cachePost(msg)
		out = append(out, msg)
	}

	sortMessagesByTime(out)

	if len(out) > limit {
		out = out[len(out)-limit:]
	}

	return out, nil
}

func (c *Client) parseSearchHitMessage(raw json.RawMessage) *ChatMessage {
	var fields []json.RawMessage

	err := json.Unmarshal(raw, &fields)
	if err != nil || len(fields) < 18 {
		return nil
	}

	convID, isDM := parseSearchGroupRef(fields[2])
	if convID == "" {
		return nil
	}

	c.rememberGroupRef(protoGroupRef{ID: convID, IsDM: isDM})

	msgID, topicID := parseSearchMessageID(fields[17])
	if msgID == "" {
		msgID = topicID
	}

	rootID := ""
	if topicID != "" && msgID != "" && topicID != msgID {
		rootID = topicID
	}

	createUsec := parseJSONInt64(fields[11])
	createMs := parseJSONInt64(fields[16])

	if createMs == 0 && createUsec > 0 {
		createMs = usecToMillis(createUsec)
	}

	if createUsec == 0 && createMs > 0 {
		createUsec = createMs * 1000
	}

	return &ChatMessage{
		ID:                 msgID,
		TopicID:            topicID,
		ConversationID:     convID,
		RootMessageID:      rootID,
		FromID:             parseSearchCreatorID(fields[4]),
		ContentText:        parseJSONString(fields[5]),
		CreateAt:           createMs,
		LastUpdateTimeUsec: createUsec,
	}
}

func parseSearchGroupRef(raw json.RawMessage) (string, bool) {
	var parts []json.RawMessage

	err := json.Unmarshal(raw, &parts)
	if err != nil || len(parts) < 2 {
		return "", false
	}

	qualified := parseJSONString(parts[0])
	id := parseJSONString(parts[1])

	return id, strings.HasPrefix(qualified, "dm/")
}

func parseSearchCreatorID(raw json.RawMessage) string {
	var parts []json.RawMessage

	err := json.Unmarshal(raw, &parts)
	if err != nil || len(parts) == 0 {
		return ""
	}

	return parseJSONString(parts[0])
}

func parseSearchMessageID(raw json.RawMessage) (string, string) {
	var parts []json.RawMessage

	err := json.Unmarshal(raw, &parts)
	if err != nil || len(parts) == 0 {
		return "", ""
	}

	msgID := parseJSONString(parts[0])
	topicID := ""

	if len(parts) >= 3 {
		var parent []json.RawMessage
		if json.Unmarshal(parts[2], &parent) == nil && len(parent) > 0 {
			topicID = parseJSONString(parent[0])
		}
	}

	return msgID, topicID
}

func parseJSONString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}

	return s
}

func parseJSONInt64(raw json.RawMessage) int64 {
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		parsed, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return parsed
		}
	}

	return 0
}

func newSearchUUID() string {
	var b [16]byte

	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08X-%04X-%04X-%04X-%012X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
