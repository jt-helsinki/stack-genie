package cli

import (
	"errors"
	"slices"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// parseOptionalFlag turns the --optional CSV (the non-interactive setup contract)
// into a validated service list: "none" disables all, a CSV selects (trimmed),
// and any unknown name is exit 2.
func TestParseOptionalFlag(test *testing.T) {
	valid := []struct {
		in   string
		want []string
	}{
		{"none", []string{}},
		{"NONE", []string{}},
		{"open-webui", []string{"open-webui"}},
		{"open-webui,odysseus", []string{"open-webui", "odysseus"}},
		{"  open-webui , odysseus  ", []string{"open-webui", "odysseus"}},
		{"open-webui,,", []string{"open-webui"}},
	}
	for _, testCase := range valid {
		got, err := parseOptionalFlag(testCase.in)
		if err != nil {
			test.Errorf("parseOptionalFlag(%q) unexpected error: %v", testCase.in, err)
			continue
		}
		if !slices.Equal(got, testCase.want) {
			test.Errorf("parseOptionalFlag(%q) = %v, want %v", testCase.in, got, testCase.want)
		}
	}

	for _, bad := range []string{"bogus", "open-webui,bogus", "llm-guard"} {
		_, err := parseOptionalFlag(bad)
		var platformErr *output.Error
		if !errors.As(err, &platformErr) || platformErr.Code != output.ExitInvalidInput {
			test.Errorf("parseOptionalFlag(%q) = %v, want exit %d", bad, err, output.ExitInvalidInput)
		}
	}
}

// optionalServicesFromFlag is the non-interactive (--json / no-TTY) resolution:
// an empty flag leaves the set unspecified (set=false, keep persisted/default);
// any value makes an explicit choice (set=true), with "none" meaning empty.
func TestOptionalServicesFromFlag(test *testing.T) {
	if set, services, err := optionalServicesFromFlag(""); set || services != nil || err != nil {
		test.Errorf("empty flag = (set=%v, %v, %v), want (false, nil, nil) — unspecified", set, services, err)
	}
	if set, services, err := optionalServicesFromFlag("none"); !set || len(services) != 0 || err != nil {
		test.Errorf("\"none\" = (set=%v, %v, %v), want (true, [], nil)", set, services, err)
	}
	set, services, err := optionalServicesFromFlag("open-webui")
	if !set || err != nil || !slices.Equal(services, []string{"open-webui"}) {
		test.Errorf("\"open-webui\" = (set=%v, %v, %v), want (true, [open-webui], nil)", set, services, err)
	}
	if _, _, err := optionalServicesFromFlag("bogus"); err == nil {
		test.Error("unknown --optional name must error (exit 2)")
	}
}
