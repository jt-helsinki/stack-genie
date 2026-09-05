package cli

import (
	"testing"
)

func TestSpecFromFlagsCarriesGraphifyModel(test *testing.T) {
	repo := "qwen3-coder"
	spec, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", graphifyModel: repo, defaultName: "demo"})
	if err != nil {
		test.Fatal(err)
	}
	if spec.GraphifyModel != repo {
		test.Fatalf("spec.GraphifyModel = %q, want %q", spec.GraphifyModel, repo)
	}
}
