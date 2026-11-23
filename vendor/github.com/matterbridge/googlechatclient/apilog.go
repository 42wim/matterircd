package googlechatclient

import (
	"net/http"
	"strings"
	"time"

	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/sirupsen/logrus"
)

func newLoggers() (*logrus.Logger, *logrus.Logger) {
	// Logger for googlechatclient internals (matching matterclient's PrefixPadding: 13).
	rootLogger := logrus.New()
	rootLogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})

	// Logger for Google Chat API calls (matching matterclient's PrefixPadding: 18 and default ErrorLevel).
	rootAPILogger := logrus.New()
	rootAPILogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 18,
		DisableColors: false,
		FullTimestamp: true,
	})
	rootAPILogger.SetLevel(logrus.ErrorLevel)

	return rootLogger, rootAPILogger
}

// apiLogTransport logs Google Chat API requests on the API logger without ever logging
// query parameters, cookies, headers, or credentials.
type apiLogTransport struct {
	next   http.RoundTripper
	logger *logrus.Entry
}

func newAPILogTransport(next http.RoundTripper, logger *logrus.Entry) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}

	return &apiLogTransport{
		next:   next,
		logger: logger,
	}
}

func (t *apiLogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	resp, err := t.next.RoundTrip(req)
	duration := time.Since(start).Round(time.Millisecond)

	// Log only host + path; query strings can contain SID, csessionid, or attachment tokens.
	target := req.URL.Host + req.URL.Path

	if err != nil {
		t.logger.Warnf("%s %s: %s (%s)", req.Method, target, err, duration)

		return resp, err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		t.logger.Warnf("%s %s: %d (%s)", req.Method, target, resp.StatusCode, duration)

		return resp, nil
	}

	// Periodic heartbeats and WebChannel event/keepalive frames would flood warn level.
	if strings.HasSuffix(req.URL.Path, "/api/heartbeat") || strings.Contains(req.URL.Path, "/webchannel/events_encoded") {
		t.logger.Tracef("%s %s: %d (%s)", req.Method, target, resp.StatusCode, duration)

		return resp, nil
	}

	// Like matterclient, matrixclient, and msteamsclient, "warn" shows API calls.
	t.logger.Warnf("%s %s: %d (%s)", req.Method, target, resp.StatusCode, duration)

	return resp, nil
}
