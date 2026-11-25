package googlechatclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

const (
	assistiveFeaturesURL = "https://peoplestack-pa.clients6.google.com/$rpc/social.people.backend.service.intelligence.proto.PeopleStackIntelligenceService/GetAssistiveFeatures"
	assistiveFeatureOp   = "peoplestack/GetAssistiveFeatures"

	assistivePresenceActive = 1
	assistivePresenceAway   = 2
	assistiveFeaturePres    = 4
	assistiveFeatureDND     = 5
)

var errWrongProjectAPIKey = errors.New("googlechatclient: API key not enabled for PeopleStack")

type assistivePresence struct {
	Presence int
	DND      bool
}

func (c *Client) ensureAPIKeys(ctx context.Context) (string, []string, error) {
	c.authMu.RLock()
	pinned := c.apiKey
	candidates := slices.Clone(c.apiKeys)
	c.authMu.RUnlock()

	if pinned != "" || len(candidates) > 0 {
		return pinned, candidates, nil
	}

	err := c.refreshXSRFToken(ctx)
	if err != nil {
		return "", nil, err
	}

	c.authMu.RLock()
	pinned = c.apiKey
	candidates = slices.Clone(c.apiKeys)
	c.authMu.RUnlock()

	if pinned == "" && len(candidates) == 0 {
		return "", nil, errors.New("googlechatclient: missing Google API key for GetAssistiveFeatures")
	}

	return pinned, candidates, nil
}

func (c *Client) fetchAssistivePresences(ctx context.Context, userIDs []string) (map[string]*assistivePresence, error) {
	if len(userIDs) == 0 {
		return map[string]*assistivePresence{}, nil
	}

	pinned, candidates, err := c.ensureAPIKeys(ctx)
	if err != nil {
		return nil, err
	}

	out := make(map[string]*assistivePresence, len(userIDs))

	for chunk := range slices.Chunk(userIDs, presenceBatchSize) {
		reqBody, encErr := encodeAssistiveFeaturesRequest(chunk)
		if encErr != nil {
			return out, encErr
		}

		respBytes, usedKey, callErr := c.doAssistiveFeaturesWithKeys(ctx, pinned, candidates, reqBody)
		if callErr != nil {
			return out, callErr
		}

		if usedKey != "" && usedKey != pinned {
			pinned = usedKey

			c.authMu.Lock()
			c.apiKey = usedKey
			c.authMu.Unlock()
		}

		parseAssistiveFeaturesResponse(respBytes, out)
	}

	return out, nil
}

func (c *Client) doAssistiveFeaturesWithKeys(ctx context.Context, pinned string, candidates []string, reqBody []byte) ([]byte, string, error) {
	if pinned != "" {
		resp, err := c.doAssistiveFeatures(ctx, pinned, reqBody)
		if err == nil {
			return resp, pinned, nil
		}

		if !errors.Is(err, errWrongProjectAPIKey) {
			return nil, "", err
		}
	}

	var lastErr error

	for _, key := range candidates {
		if key == "" || key == pinned {
			continue
		}

		resp, err := c.doAssistiveFeatures(ctx, key, reqBody)
		if err == nil {
			return resp, key, nil
		}

		lastErr = err
		if !errors.Is(err, errWrongProjectAPIKey) {
			return nil, "", err
		}
	}

	return nil, "", lastErr
}

func encodeAssistiveFeaturesRequest(userIDs []string) ([]byte, error) {
	targets := make([][]any, 0, len(userIDs))
	for _, id := range userIDs {
		if id != "" {
			targets = append(targets, []any{2, id})
		}
	}

	payload := []any{
		[]any{1, "1"},
		[]any{
			[]any{
				targets,
				[]int{assistiveFeaturePres, assistiveFeatureDND},
			},
		},
	}

	return json.Marshal(payload)
}

func (c *Client) doAssistiveFeatures(ctx context.Context, apiKey string, reqBody []byte) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		respBytes, status, header, err := c.doAssistiveFeaturesOnce(ctx, apiKey, reqBody)
		if err == nil && status > 0 && status < http.StatusBadRequest {
			return respBytes, nil
		}

		if err == nil && status == http.StatusForbidden && isWrongProjectBody(respBytes) {
			return nil, fmt.Errorf("%w: HTTP %d: %s", errWrongProjectAPIKey, status, truncateBody(respBytes))
		}

		var resp *http.Response
		if status > 0 {
			resp = &http.Response{
				StatusCode: status,
				Header:     header,
			}
		}

		shouldRetry, retryErr := c.HandleRetry(ctx, assistiveFeatureOp, err, attempt, maxAPIRetries, resp)
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
			return nil, fmt.Errorf("googlechatclient: POST %s returned HTTP %d: %s", assistiveFeatureOp, status, truncateBody(respBytes))
		}

		return nil, fmt.Errorf("googlechatclient: POST %s failed with nil response", assistiveFeatureOp)
	}
}

func isWrongProjectBody(body []byte) bool {
	return bytes.Contains(body, []byte("has not been used in project")) ||
		bytes.Contains(body, []byte("API_KEY_INVALID")) ||
		bytes.Contains(body, []byte("API_KEY_SERVICE_BLOCKED"))
}

func (c *Client) doAssistiveFeaturesOnce(ctx context.Context, apiKey string, reqBody []byte) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, assistiveFeaturesURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, 0, nil, err
	}

	c.authMu.RLock()

	sapisid := c.tokenState.Cookies["SAPISID"]
	if sapisid == "" {
		sapisid = c.tokenState.Cookies["__Secure-3PAPISID"]
	}

	c.authMu.RUnlock()

	req.Header.Set("Content-Type", "application/json+protobuf")
	req.Header.Set("X-Goog-Api-Key", apiKey)
	c.applyCookieAndAuthUserHeaders(req)

	if sapisid != "" {
		req.Header.Set("Authorization", sapisidAuthHeader(sapisid, chatOriginURL, time.Now()))
	}

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

func parseAssistiveFeaturesResponse(raw []byte, out map[string]*assistivePresence) {
	var root []json.RawMessage

	err := json.Unmarshal(bytes.TrimSpace(raw), &root)
	if err != nil {
		return
	}

	if len(root) > 4 {
		parseAssistivePresenceList(root[4], out)
	}

	if len(root) > 5 {
		parseAssistiveDNDList(root[5], out)
	}
}

func parseAssistivePresenceList(raw json.RawMessage, out map[string]*assistivePresence) {
	var entries [][]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return
	}

	for _, entry := range entries {
		if len(entry) < 3 {
			continue
		}

		uid := parseAssistiveTargetID(entry[1])
		if uid == "" {
			continue
		}

		var detail []json.RawMessage
		if json.Unmarshal(entry[2], &detail) != nil || len(detail) < 2 {
			continue
		}

		state := int(parseJSONInt64(detail[1]))
		if state == 0 {
			continue
		}

		ap := ensureAssistiveEntry(out, uid)
		ap.Presence = state
	}
}

func parseAssistiveDNDList(raw json.RawMessage, out map[string]*assistivePresence) {
	var entries [][]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return
	}

	nowUsec := time.Now().UnixMicro()

	for _, entry := range entries {
		if len(entry) < 3 {
			continue
		}

		uid := parseAssistiveTargetID(entry[1])
		if uid == "" {
			continue
		}

		var detail []json.RawMessage
		if json.Unmarshal(entry[2], &detail) != nil || len(detail) < 2 {
			continue
		}

		var dndWrap []json.RawMessage
		if json.Unmarshal(detail[1], &dndWrap) != nil || len(dndWrap) == 0 {
			continue
		}

		var dndInfo []json.RawMessage
		if json.Unmarshal(dndWrap[0], &dndInfo) != nil || len(dndInfo) == 0 {
			continue
		}

		dndState := parseJSONInt64(dndInfo[0])

		expiryUsec := int64(0)
		if len(dndInfo) >= 2 {
			expiryUsec = parseJSONInt64(dndInfo[1])
		}

		if dndState == protoDndActive && (expiryUsec == 0 || expiryUsec > nowUsec) {
			ap := ensureAssistiveEntry(out, uid)
			ap.DND = true
		}
	}
}

func parseAssistiveTargetID(raw json.RawMessage) string {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) < 2 {
		return ""
	}

	return parseJSONString(parts[1])
}

func ensureAssistiveEntry(out map[string]*assistivePresence, uid string) *assistivePresence {
	ap := out[uid]
	if ap == nil {
		ap = &assistivePresence{}
		out[uid] = ap
	}

	return ap
}
