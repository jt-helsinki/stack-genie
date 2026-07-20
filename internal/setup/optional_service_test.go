package setup

import (
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

// TestServiceStatusEnabled: a core service is always enabled; an optional service is
// enabled unless it is in the "disabled" state.
func TestServiceStatusEnabled(test *testing.T) {
	cases := []struct {
		name   string
		status ServiceStatus
		want   bool
	}{
		{name: "core running", status: ServiceStatus{Optional: false, State: "running"}, want: true},
		{name: "core disabled-state still enabled", status: ServiceStatus{Optional: false, State: "disabled"}, want: true},
		{name: "optional running", status: ServiceStatus{Optional: true, State: "running"}, want: true},
		{name: "optional disabled", status: ServiceStatus{Optional: true, State: "disabled"}, want: false},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			if got := testCase.status.Enabled(); got != testCase.want {
				test.Errorf("Enabled() = %v, want %v for %+v", got, testCase.want, testCase.status)
			}
		})
	}
}

// TestOptionalServiceLabel: there are currently no optional host services, so every
// name (known or not) yields an empty label.
func TestOptionalServiceLabel(test *testing.T) {
	for _, name := range []string{"", "litellm", "openwebui", "does-not-exist"} {
		if label := OptionalServiceLabel(name); label != "" {
			test.Errorf("OptionalServiceLabel(%q) = %q, want empty (no optional host services)", name, label)
		}
	}
}

// TestOptionalSetWith: the returned set only ever contains names in the optional
// universe (currently empty), so both enable and disable yield an empty set.
func TestOptionalSetWith(test *testing.T) {
	if got := optionalSetWith(nil, "foo", true); len(got) != 0 {
		test.Errorf("optionalSetWith(nil, foo, enable) = %v, want empty (foo not in the optional universe)", got)
	}
	if got := optionalSetWith([]string{"foo"}, "foo", false); len(got) != 0 {
		test.Errorf("optionalSetWith([foo], foo, disable) = %v, want empty", got)
	}
}

// TestOptionalServiceEnabled reflects the persisted runtime.yaml optional set: absent
// when there is no runtime.yaml, and driven by info.OptionalServices once persisted.
func TestOptionalServiceEnabled(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	// No runtime.yaml yet → nothing is enabled.
	if optionalServiceEnabled("foo") {
		test.Fatal("with no runtime.yaml, no optional service should report enabled")
	}

	// Persist a runtime.yaml that lists "foo" as enabled.
	if err := runtime.Persist(&runtime.Info{OptionalServices: []string{"foo"}}); err != nil {
		test.Fatalf("persist runtime.yaml: %v", err)
	}
	if !optionalServiceEnabled("foo") {
		test.Error("optionalServiceEnabled(foo) = false, want true (present in the persisted set)")
	}
	if optionalServiceEnabled("bar") {
		test.Error("optionalServiceEnabled(bar) = true, want false (absent from the persisted set)")
	}
}
