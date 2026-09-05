package views

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestParsePortsForSpec covers the comma-separated ports field parser: blank yields
// no mappings, a bare port maps host==guest, HOST:GUEST splits, and out-of-range /
// non-numeric entries error.
func TestParsePortsForSpec(test *testing.T) {
	cases := []struct {
		name      string
		value     string
		wantPorts []struct{ host, guest int }
		wantErr   bool
	}{
		{name: "blank", value: "", wantPorts: nil},
		{name: "whitespace", value: "   ", wantPorts: nil},
		{name: "single host==guest", value: "8080", wantPorts: []struct{ host, guest int }{{8080, 8080}}},
		{name: "host:guest", value: "9000:3000", wantPorts: []struct{ host, guest int }{{9000, 3000}}},
		{name: "multiple trimmed", value: " 8080 , 9000:3000 ", wantPorts: []struct{ host, guest int }{{8080, 8080}, {9000, 3000}}},
		{name: "non-numeric", value: "abc", wantErr: true},
		{name: "host out of range", value: "70000", wantErr: true},
		{name: "guest out of range", value: "8080:99999", wantErr: true},
		{name: "zero port", value: "0", wantErr: true},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			ports, err := parsePortsForSpec(testCase.value)
			if testCase.wantErr {
				if err == nil {
					test.Fatalf("parsePortsForSpec(%q) = %v, want error", testCase.value, ports)
				}
				return
			}
			if err != nil {
				test.Fatalf("parsePortsForSpec(%q) unexpected error: %v", testCase.value, err)
			}
			if len(ports) != len(testCase.wantPorts) {
				test.Fatalf("parsePortsForSpec(%q) = %v, want %d mappings", testCase.value, ports, len(testCase.wantPorts))
			}
			for index, want := range testCase.wantPorts {
				if ports[index].Host != want.host || ports[index].Guest != want.guest {
					test.Errorf("mapping %d = %+v, want host=%d guest=%d", index, ports[index], want.host, want.guest)
				}
			}
		})
	}
}

// TestValidateCPUsField: blank OK, a valid small count OK, non-numeric/zero error.
func TestValidateCPUsField(test *testing.T) {
	if err := validateCPUsField(""); err != nil {
		test.Errorf("blank vCPUs should be allowed, got %v", err)
	}
	if err := validateCPUsField("1"); err != nil {
		test.Errorf("1 vCPU should be valid on any host, got %v", err)
	}
	if err := validateCPUsField("abc"); err == nil {
		test.Error("non-numeric vCPUs should error")
	}
	if err := validateCPUsField("0"); err == nil {
		test.Error("zero vCPUs should error")
	}
}

// TestValidateMemoryField: blank OK, a modest value OK, garbage/below-minimum error.
func TestValidateMemoryField(test *testing.T) {
	if err := validateMemoryField(""); err != nil {
		test.Errorf("blank memory should be allowed, got %v", err)
	}
	if err := validateMemoryField("1"); err != nil {
		test.Errorf("1 GB should be valid on a normal host, got %v", err)
	}
	if err := validateMemoryField("abc"); err == nil {
		test.Error("non-numeric memory should error")
	}
	if err := validateMemoryField("0"); err == nil {
		test.Error("0 GB (below the boot minimum) should error")
	}
}

// TestValidateAppPortField: blank OK (auto-assign), a valid host port OK, out-of-range
// / non-numeric error.
func TestValidateAppPortField(test *testing.T) {
	if err := validateAppPortField(""); err != nil {
		test.Errorf("blank app port should be allowed (auto-assign), got %v", err)
	}
	if err := validateAppPortField("  "); err != nil {
		test.Errorf("whitespace-only app port should be allowed, got %v", err)
	}
	if err := validateAppPortField("8080"); err != nil {
		test.Errorf("8080 should be a valid host port, got %v", err)
	}
	if err := validateAppPortField("0"); err == nil {
		test.Error("port 0 should error")
	}
	if err := validateAppPortField("70000"); err == nil {
		test.Error("port above 65535 should error")
	}
	if err := validateAppPortField("nope"); err == nil {
		test.Error("non-numeric app port should error")
	}
}

// TestCreateStartDir: a non-empty argument is returned verbatim; an empty one falls
// back to the working directory.
func TestCreateStartDir(test *testing.T) {
	if got := createStartDir("/some/where"); got != "/some/where" {
		test.Errorf("createStartDir(%q) = %q, want it returned verbatim", "/some/where", got)
	}
	wd, err := os.Getwd()
	if err != nil {
		test.Fatalf("Getwd: %v", err)
	}
	if got := createStartDir(""); got != wd {
		test.Errorf("createStartDir(\"\") = %q, want the working directory %q", got, wd)
	}
}

// TestCreateWizardBackNavigation walks the early static steps forward, then uses
// shift+tab (prev) to step back one at a time, verifying the transitions and that
// back at the first step is a no-op. Complements the forward-flow wizard tests.
func TestCreateWizardBackNavigation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	target := filepath.Join(base, "demo-ws")

	wizard := NewCreate(base, 24, 18, nil)
	wizard.SetSize(80, 24)
	enter := func() { wizard.Update(tea.KeyMsg{Type: tea.KeyEnter}) }
	back := func() { wizard.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) }

	wizard.location.input.SetValue(target)
	enter() // location -> name
	if wizard.step != stepName {
		test.Fatalf("after location enter, step = %d, want stepName (%d)", wizard.step, stepName)
	}
	wizard.name.input.SetValue("demo-ws")
	enter() // name -> OS
	if wizard.step != stepOS {
		test.Fatalf("after name enter, step = %d, want stepOS (%d)", wizard.step, stepOS)
	}
	enter() // OS -> shell
	if wizard.step != stepShell {
		test.Fatalf("after OS enter, step = %d, want stepShell (%d)", wizard.step, stepShell)
	}

	back() // shell -> OS
	if wizard.step != stepOS {
		test.Fatalf("after back, step = %d, want stepOS (%d)", wizard.step, stepOS)
	}
	back() // OS -> name
	if wizard.step != stepName {
		test.Fatalf("after back, step = %d, want stepName (%d)", wizard.step, stepName)
	}
	back() // name -> location
	if wizard.step != stepLocation {
		test.Fatalf("after back, step = %d, want stepLocation (%d)", wizard.step, stepLocation)
	}
	back() // location -> location (no-op at the first step)
	if wizard.step != stepLocation {
		test.Fatalf("back at the first step must be a no-op, step = %d", wizard.step)
	}
}
