package cli

import (
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

func TestSpecFromFlagsCarriesApps(test *testing.T) {
	spec, err := specFromFlags("demo", "ubuntu", nil, nil, []string{"openwebui"}, "demo")
	if err != nil {
		test.Fatal(err)
	}
	if len(spec.Apps) != 1 || spec.Apps[0] != "openwebui" {
		test.Fatalf("spec.Apps = %v, want [openwebui]", spec.Apps)
	}
}

func TestSpecFromFlagsAppsDefaultEmpty(test *testing.T) {
	spec, err := specFromFlags("demo", "ubuntu", nil, nil, nil, "demo")
	if err != nil {
		test.Fatal(err)
	}
	if len(spec.Apps) != 0 {
		test.Fatalf("spec.Apps = %v, want empty (apps opt-in)", spec.Apps)
	}
}

func TestSpecFromFlagsRejectsUnknownApp(test *testing.T) {
	_, err := specFromFlags("demo", "ubuntu", nil, nil, []string{"nope"}, "demo")
	platformErr, ok := err.(*output.Error)
	if !ok || platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("err = %v, want exit-2 *output.Error", err)
	}
}

func TestSeedSpecAppsEmptyByDefault(test *testing.T) {
	spec := seedSpec("demo", "ubuntu", nil, nil, nil, "demo")
	if len(spec.Apps) != 0 {
		test.Fatalf("seedSpec apps = %v, want empty", spec.Apps)
	}
}
