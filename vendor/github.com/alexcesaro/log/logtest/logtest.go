// Package logtest provides utilities for logging testing.
package logtest

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/alexcesaro/log"
)

// Messages is a map of log messages.
var Messages = map[log.Level]string{
	log.Emergency: "Emergency test message",
	log.Alert:     "Alert test message",
	log.Critical:  "Critical test message",
	log.Error:     "Error test message",
	log.Warning:   "Warning test message",
	log.Notice:    "Notice test message",
	log.Info:      "Info test message",
	log.Debug:     "Debug test message",
}

// AssertContains returns true if a buffer contains a line containing message.
func AssertContains(t *testing.T, out *bytes.Buffer, message string) {
	if !strings.Contains(out.String(), message) {
		t.Errorf("Logs do not contain %q, content: %q", message, out.String())
	}
}

// AssertNotContain returns true if a buffer does not contain a line containing message.
func AssertNotContain(t *testing.T, out *bytes.Buffer, message string) {
	if strings.Contains(out.String(), message) {
		t.Errorf("Logs contain %q, content: %q", message, out.String())
	}
}

// AssertLineCount returns the number of log line a buffer contains.
func AssertLineCount(t *testing.T, out *bytes.Buffer, expectedCount int) {
	count := 0
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		count++
	}

	if err := scanner.Err(); err != nil {
		t.Error(err)
	}

	if expectedCount != count {
		t.Errorf("Invalid number of logs lines, got %d, expected %d", count, expectedCount)
	}
}
