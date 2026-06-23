package ui

import (
	"strings"
	"testing"
)

// Table renders the header cells and the row values into the bordered table.
func TestTableContainsHeadersAndCells(t *testing.T) {
	rendered := Table(
		[]string{"NAME", "STATUS"},
		[][]string{{"alpha", "running"}, {"beta", "stopped"}},
	)
	for _, want := range []string{"NAME", "STATUS", "alpha", "running", "beta", "stopped"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("table missing %q:\n%s", want, rendered)
		}
	}
}
