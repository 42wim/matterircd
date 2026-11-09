package matrixclient

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

type timedError struct {
	err      error
	duration time.Duration
}

func (e *timedError) Error() string {
	return fmt.Sprintf("%s (%s)", e.err.Error(), e.duration.Round(time.Millisecond))
}

func (e *timedError) Unwrap() error {
	return e.err
}

func (e *timedError) Timeout() bool {
	if netErr, ok := errors.AsType[net.Error](e.err); ok {
		return netErr.Timeout()
	}

	return false
}

func (e *timedError) Temporary() bool {
	if netErr, ok := errors.AsType[net.Error](e.err); ok {
		return netErr.Temporary() //nolint:staticcheck
	}

	return false
}

type apiLogTransport struct {
	next          http.RoundTripper
	logger        *logrus.Entry
	initialSynced atomic.Bool
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
		duration := time.Since(start)

		t.logger.Warnf("%s %s: %s (%s)", req.Method, req.URL.Path, err, duration.Round(time.Millisecond))

		return resp, &timedError{
			err:      err,
			duration: duration,
		}
	}

	// Only the path is logged, the query string can carry credentials.
	if resp.StatusCode >= http.StatusBadRequest {
		t.logger.Warnf("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond))

		return resp, nil
	}

	// Like matterclient, "warn" shows the API calls. The /sync long poll runs
	// continuously and would flood that level, so it is only logged at trace
	// after the initial sync completes.
	if strings.HasSuffix(req.URL.Path, "/sync") {
		duration := time.Since(start).Round(time.Millisecond)

		if !t.initialSynced.Swap(true) {
			t.logger.Warnf("%s %s (initial sync): %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration)

			return resp, nil
		}

		t.logger.Tracef("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration)

		return resp, nil
	}

	t.logger.Warnf("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond))

	return resp, nil
}
