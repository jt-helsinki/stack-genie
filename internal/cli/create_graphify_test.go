package cli

import (
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/hf"
)

func TestSpecFromFlagsCarriesGraphifyModel(test *testing.T) {
	repo := "mlx-community/Qwen2.5-7B-Instruct-4bit"
	spec, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", graphifyModel: repo, defaultName: "demo"})
	if err != nil {
		test.Fatal(err)
	}
	if spec.GraphifyModel != repo {
		test.Fatalf("spec.GraphifyModel = %q, want %q", spec.GraphifyModel, repo)
	}
}

func TestGraphifyModelOptionsLeadWithNone(test *testing.T) {
	curated := []hf.CuratedModel{
		{Name: "Qwen2.5-7B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit", Size: "~4 GB"},
		{Name: "Llama-3.2-3B-Instruct-4bit", Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit"},
	}
	options := graphifyModelOptions(curated)
	if len(options) != 3 || options[0].Value != "" {
		test.Fatalf("graphifyModelOptions = %v, want leading (none) + 2 models", options)
	}
	// The option value is the repo id (not the short name).
	if options[1].Value != "mlx-community/Qwen2.5-7B-Instruct-4bit" {
		test.Fatalf("graphifyModelOptions[1].Value = %q, want the repo id", options[1].Value)
	}
}
