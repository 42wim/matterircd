package rocketchatclient

import "time"

const (
	defaultDialTimeout       = 10 * time.Second
	defaultSentLRUSize       = 512
	defaultMsgLRUSize        = 2048
	defaultReactionLRUSize   = 1024
	defaultHTTPClientTimeout = 30 * time.Second
)

// Option configures New.
type Option func(*options)

type options struct {
	skipTLSVerify   bool
	noTLS           bool
	timeout         time.Duration
	userAgent       string
	sentLRUSize     int
	msgLRUSize      int
	reactionLRUSize int
}

func defaultOptions() options {
	return options{
		timeout:         defaultDialTimeout,
		sentLRUSize:     defaultSentLRUSize,
		msgLRUSize:      defaultMsgLRUSize,
		reactionLRUSize: defaultReactionLRUSize,
	}
}

// WithSkipTLSVerify disables TLS certificate verification for REST and WebSocket connections.
func WithSkipTLSVerify(skip bool) Option {
	return func(o *options) { o.skipTLSVerify = skip }
}

// WithNoTLS forces plain HTTP and WS instead of HTTPS and WSS when no scheme is specified.
func WithNoTLS(noTLS bool) Option {
	return func(o *options) { o.noTLS = noTLS }
}

// WithTimeout sets the dial and TLS handshake timeout.
func WithTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// WithUserAgent sets the User-Agent header for REST and WebSocket requests.
func WithUserAgent(ua string) Option {
	return func(o *options) { o.userAgent = ua }
}

// WithLRUSizes overrides the bounded capacities for sent ID, message body, and reaction snapshot caches.
func WithLRUSizes(sentIDs, messages, reactions int) Option {
	return func(o *options) {
		if sentIDs > 0 {
			o.sentLRUSize = sentIDs
		}

		if messages > 0 {
			o.msgLRUSize = messages
		}

		if reactions > 0 {
			o.reactionLRUSize = reactions
		}
	}
}
