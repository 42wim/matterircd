package matrixclient

import (
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"
)

const defaultTimeout = 10 * time.Second

type splitTransport struct {
	api  *http.Transport
	sync *http.Transport
}

// newBaseTransport returns the base HTTP transport. As in matterclient, dialing and the
// TLS handshake are bounded by timeout, so an unreachable server fails fast.
func newBaseTransport(o *options, responseTimeout time.Duration) *http.Transport {
	timeout := o.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	return &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: o.skipTLSVerify, //nolint:gosec
		},
		Proxy: http.ProxyFromEnvironment,

		// https://github.com/golang/go/issues/39299
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: responseTimeout,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 15 * time.Second,
			PingTimeout:     5 * time.Second,
		},
	}
}

func (s *splitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/sync") {
		return s.sync.RoundTrip(req)
	}

	return s.api.RoundTrip(req)
}

// newTransport returns a transport that isolates long-polling /sync requests
// onto a separate connection pool from normal API calls. As in matterclient,
// dialing and the TLS handshake are bounded by timeout, so an unreachable
// server fails fast.
func newTransport(o *options) http.RoundTripper {
	return &splitTransport{
		api:  newBaseTransport(o, 20*time.Second),
		sync: newBaseTransport(o, 45*time.Second),
	}
}
