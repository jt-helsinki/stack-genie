package views

import (
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
)

// noTest is a ModelTester stub for tests that don't exercise the test-probe path —
// it always reports OK without any network call. Shared by the Cloud Models tests
// and the cross-view degrade-gracefully regression tests.
func noTest(model string) (litellm.TestResult, error) {
	return litellm.TestResult{Model: model, OK: true}, nil
}

// renderedHeight counts the lines in a rendered view (for asserting a view fills or
// stays within its allotted height).
func renderedHeight(rendered string) int {
	return strings.Count(rendered, "\n") + 1
}

// stripANSI removes SGR escape sequences so a "blank" padding line (which may carry a
// reset) is recognised as visually empty. Shared by any view test asserting on plain
// rendered text (listtable, the create wizard's select steps, …).
func stripANSI(line string) string {
	var out strings.Builder
	for {
		start := strings.IndexByte(line, '\x1b')
		if start < 0 {
			out.WriteString(line)
			break
		}
		out.WriteString(line[:start])
		end := strings.IndexByte(line[start:], 'm')
		if end < 0 {
			break
		}
		line = line[start+end+1:]
	}
	return out.String()
}
