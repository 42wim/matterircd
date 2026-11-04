package matrixclient

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

const defaultTimeout = 10 * time.Second

// newTransport returns the base HTTP transport. As in matterclient, dialing and the
// TLS handshake are bounded by timeout, so an unreachable server fails fast. There is
// no overall request timeout here: the sync long-poll is bounded by the mautrix client timeout.
func newTransport(o *options) *http.Transport {
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
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: 1 * time.Second,

		DisableCompression: true,
	}
}
