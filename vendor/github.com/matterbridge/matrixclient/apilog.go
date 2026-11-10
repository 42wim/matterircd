package matrixclient

import (
	"errors"
	"fmt"
	"io"
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

type timedBody struct {
	io.ReadCloser

	start    time.Time
	readTime time.Duration
	bytes    int64
	closed   bool
	onClose  func(bytes int64, readTime, decodeTime, totalTime time.Duration)
}

func (b *timedBody) Read(p []byte) (int, error) {
	start := time.Now()

	n, err := b.ReadCloser.Read(p)

	b.readTime += time.Since(start)
	b.bytes += int64(n)

	return n, err
}

func (b *timedBody) Close() error {
	if b.closed {
		return nil
	}

	b.closed = true

	if b.onClose != nil {
		total := time.Since(b.start)
		decodeTime := max(0, total-b.readTime)

		b.onClose(b.bytes, b.readTime, decodeTime, total)
	}

	return b.ReadCloser.Close()
}

func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.2f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.2f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// apiLogTransport logs the Matrix API calls on the API logger, see SetLogAPICalls.
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

//nolint:funlen
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

	isSync := strings.HasSuffix(req.URL.Path, "/sync")
	isInitialSync := isSync && !t.initialSynced.Load()

	duration := time.Since(start).Round(time.Millisecond)

	// Only wrap sync responses to measure body download and JSON decode times.
	// For recurring syncs, only wrap if trace logging is active.
	traceEnabled := t.logger != nil && t.logger.Logger.IsLevelEnabled(logrus.TraceLevel)
	if isSync && resp.StatusCode == http.StatusOK && resp.Body != nil && (isInitialSync || traceEnabled) {
		bodyStart := time.Now()

		resp.Body = &timedBody{
			ReadCloser: resp.Body,
			start:      bodyStart,
			onClose: func(bytes int64, readTime, decodeTime, totalTime time.Duration) {
				if isInitialSync {
					t.logger.Warnf("%s %s (initial sync body): %s in %s (download: %s, decode: %s)",
						req.Method,
						req.URL.Path,
						formatBytes(bytes),
						totalTime.Round(time.Millisecond),
						readTime.Round(time.Millisecond),
						decodeTime.Round(time.Millisecond),
					)

					return
				}

				t.logger.Tracef("%s %s body: %s in %s (download: %s, decode: %s)",
					req.Method,
					req.URL.Path,
					formatBytes(bytes),
					totalTime.Round(time.Millisecond),
					readTime.Round(time.Millisecond),
					decodeTime.Round(time.Millisecond),
				)
			},
		}
	}

	// Only the path is logged, the query string can carry credentials.
	if resp.StatusCode >= http.StatusBadRequest {
		t.logger.Warnf("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration)

		return resp, nil
	}

	// Like matterclient, "warn" shows the API calls. The /sync long poll runs
	// continuously and would flood that level, so it is only logged at trace
	// after the initial sync completes.
	if isSync {
		if isInitialSync && resp.StatusCode == http.StatusOK {
			t.initialSynced.Store(true)

			t.logger.Warnf("%s %s (initial sync headers): %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration)

			return resp, nil
		}

		t.logger.Tracef("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration)

		return resp, nil
	}

	t.logger.Warnf("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration)

	return resp, nil
}
