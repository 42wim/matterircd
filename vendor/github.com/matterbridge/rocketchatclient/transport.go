package rocketchatclient

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

func newTransport(o *options) *http.Transport {
	timeout := o.timeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}

	return &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: o.skipTLSVerify, //nolint:gosec
		},
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 45 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	}
}

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
	if err != nil {
		t.logger.Warnf("%s %s: %s (%s)", req.Method, req.URL.Path, err, duration)

		return resp, err
	}

	// Only log the path, never query parameters which may carry identifiers or tokens.
	t.logger.Warnf("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration)

	return resp, nil
}
