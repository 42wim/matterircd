package matrixclient

import (
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// apiLogTransport logs the Matrix API calls on the API logger, see SetLogAPICalls.
type apiLogTransport struct {
	next   http.RoundTripper
	logger *logrus.Entry
}

func newAPILogTransport(next http.RoundTripper, logger *logrus.Entry) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}

	return &apiLogTransport{next: next, logger: logger}
}

func (t *apiLogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		t.logger.Warnf("%s %s: %s", req.Method, req.URL.Path, err)

		return resp, err
	}

	// Only the path is logged, the query string can carry credentials.
	if resp.StatusCode >= http.StatusBadRequest {
		t.logger.Warnf("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond))

		return resp, nil
	}

	t.logger.Infof("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond))

	return resp, nil
}
