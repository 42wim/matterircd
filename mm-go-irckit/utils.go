package irckit

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
)

// sleepContext sleeps for d. It returns false as soon as ctx is done.
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// regexpCache holds the compiled regular expressions of the configuration
// (pattern -> *regexp.Regexp, nil for patterns that don't compile).
var regexpCache sync.Map

// compileCached compiles pattern only once, it's used on the hot path of every
// incoming message. Patterns come from the configuration file and aren't
// validated, so an invalid one is logged once and never matches.
func compileCached(pattern string) *regexp.Regexp {
	if v, ok := regexpCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		logger.Errorf("ignoring invalid regular expression %q: %v", pattern, err)
	}

	regexpCache.Store(pattern, re)

	return re
}

func stringInRegexp(a string, list []string) bool {
	for _, entry := range list {
		if re := compileCached(entry); re != nil && re.MatchString(a) {
			return true
		}
	}

	return false
}

func removeStringInSlice(a string, list []string) []string {
	newlist := []string{}
	for _, b := range list {
		if b != a {
			newlist = append(newlist, b)
		}
	}
	return newlist
}

// Sanitize nick: replace IRC characters with special meanings with "-"
func sanitizeNick(nick string) string {
	sanitize := func(r rune) rune {
		if strings.ContainsRune("!+%@&#$:'\"?*, ", r) {
			return '-'
		}
		return r
	}
	return strings.Map(sanitize, nick)
}
