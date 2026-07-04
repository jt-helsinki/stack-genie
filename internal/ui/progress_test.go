package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestProgressBarUpdate renders a meter + percent + byte counts and overwrites in place.
func TestProgressBarUpdate(test *testing.T) {
	var buf bytes.Buffer
	NewProgressBar(&buf, "pulling qwen3").Update(2_000_000_000, 4_000_000_000, "downloading")
	out := buf.String()
	for _, want := range []string{"pulling qwen3", "50%", "GiB", "█", "░"} {
		if !strings.Contains(out, want) {
			test.Errorf("progress line missing %q: %q", want, out)
		}
	}
	if !strings.HasPrefix(out, "\r") {
		test.Errorf("progress must start with \\r to overwrite in place: %q", out)
	}
}

// TestProgressBarStatusOnly renders a status-only line (no meter) when total is unknown.
func TestProgressBarStatusOnly(test *testing.T) {
	var buf bytes.Buffer
	NewProgressBar(&buf, "pulling qwen3").Update(0, 0, "pulling manifest")
	out := buf.String()
	if !strings.Contains(out, "pulling manifest") {
		test.Errorf("status-only line missing status: %q", out)
	}
	if strings.Contains(out, "█") {
		test.Errorf("status-only line must not draw a meter: %q", out)
	}
}

// TestProgressBarFinish prints a final labelled line terminated by a newline.
func TestProgressBarFinish(test *testing.T) {
	var buf bytes.Buffer
	NewProgressBar(&buf, "pulling qwen3").Finish(nil)
	out := buf.String()
	if !strings.Contains(out, "pulling qwen3") || !strings.HasSuffix(out, "\n") {
		test.Errorf("finish should print a final labelled line ending in newline: %q", out)
	}
}
