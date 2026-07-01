package cli

import (
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
)

func TestSpecFromFlagsCarriesGraphifyModel(test *testing.T) {
	spec, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", graphifyModel: "qwen2.5-coder:7b", defaultName: "demo"})
	if err != nil {
		test.Fatal(err)
	}
	if spec.GraphifyModel != "qwen2.5-coder:7b" {
		test.Fatalf("spec.GraphifyModel = %q, want qwen2.5-coder:7b", spec.GraphifyModel)
	}
}

func TestSplitAndJoinModelRef(test *testing.T) {
	cases := []struct {
		ref, wantName, wantTag, wantRejoin string
	}{
		{"qwen2.5-coder:7b", "qwen2.5-coder", "7b", "qwen2.5-coder:7b"},
		{"llama3.2", "llama3.2", "", "llama3.2"},
		{"", "", "", ""},
		{"  llama3.2:1b  ", "llama3.2", "1b", "llama3.2:1b"},
	}
	for _, tc := range cases {
		name, tag := splitModelRef(tc.ref)
		if name != tc.wantName || tag != tc.wantTag {
			test.Errorf("splitModelRef(%q) = (%q,%q), want (%q,%q)", tc.ref, name, tag, tc.wantName, tc.wantTag)
		}
		if got := joinModelRef(name, tag); got != tc.wantRejoin {
			test.Errorf("joinModelRef(%q,%q) = %q, want %q", name, tag, got, tc.wantRejoin)
		}
	}
}

func TestJoinModelRefNoneYieldsEmpty(test *testing.T) {
	if got := joinModelRef("", "latest"); got != "" {
		test.Errorf("joinModelRef(none) = %q, want empty", got)
	}
}

func TestGraphifyTagOptions(test *testing.T) {
	library := []ollama.LibraryModel{
		{Name: "qwen2.5-coder", Tags: []ollama.LibraryTag{{Name: "7b"}, {Name: "3b"}}},
		{Name: "embed-only"}, // no tags
	}
	// No model selected → a single "(none)" option (empty value).
	none := graphifyTagOptions(library, "")
	if len(none) != 1 || none[0].Value != "" {
		test.Fatalf("graphifyTagOptions(none) = %v, want single empty option", none)
	}
	// A model with tags → its tags verbatim.
	tags := graphifyTagOptions(library, "qwen2.5-coder")
	if len(tags) != 2 || tags[0].Value != "7b" || tags[1].Value != "3b" {
		test.Fatalf("graphifyTagOptions(qwen) = %v, want [7b 3b]", tags)
	}
	// A tag-less (or unknown) model → the "latest" fallback.
	fallback := graphifyTagOptions(library, "embed-only")
	if len(fallback) != 1 || fallback[0].Value != "latest" {
		test.Fatalf("graphifyTagOptions(embed-only) = %v, want [latest]", fallback)
	}
}

func TestGraphifyModelOptionsLeadWithNone(test *testing.T) {
	library := []ollama.LibraryModel{{Name: "llama3.2"}, {Name: "qwen2.5"}}
	options := graphifyModelOptions(library)
	if len(options) != 3 || options[0].Value != "" {
		test.Fatalf("graphifyModelOptions = %v, want leading (none) + 2 models", options)
	}
}

func TestModelInstalled(test *testing.T) {
	installed := []ollama.Model{{Name: "llama3.2:latest"}, {Name: "qwen2.5-coder:7b"}}
	if !modelInstalled(installed, "qwen2.5-coder:7b") {
		test.Error("exact ref should be installed")
	}
	if !modelInstalled(installed, "llama3.2") {
		test.Error("bare name should match name:latest")
	}
	if modelInstalled(installed, "mistral") {
		test.Error("absent model should not be installed")
	}
}

// pullGraphifyModelIfAbsent skips the pull entirely when the model is already
// installed (no Pull call, no warning).
func TestPullGraphifyModelSkipsWhenInstalled(test *testing.T) {
	fake := &ollama.Fake{ListModels: []ollama.Model{{Name: "llama3.2:latest"}}}
	restore := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restore }()

	emitter := &output.Emitter{JSON: true}
	warnings := pullGraphifyModelIfAbsent(emitter, "llama3.2")
	if len(warnings) != 0 {
		test.Fatalf("warnings = %v, want none (already installed)", warnings)
	}
	if fake.PulledNames != nil {
		test.Fatalf("Pull should not run for an installed model; pulled %v", fake.PulledNames)
	}
}

// A blank model ref is a no-op (Graphify has no configured model).
func TestPullGraphifyModelBlankNoOp(test *testing.T) {
	fake := &ollama.Fake{}
	restore := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restore }()

	if warnings := pullGraphifyModelIfAbsent(&output.Emitter{JSON: true}, "  "); warnings != nil {
		test.Fatalf("blank ref should be a no-op, got %v", warnings)
	}
	if fake.PulledNames != nil {
		test.Fatalf("blank ref must not pull; pulled %v", fake.PulledNames)
	}
}

// An absent model is pulled and then registered in the gateway.
func TestPullGraphifyModelPullsAndRegisters(test *testing.T) {
	fake := &ollama.Fake{}
	restoreClient := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restoreClient }()

	registrar := &fakeModelRegistrar{}
	restoreReg := modelRegistrarFactory
	modelRegistrarFactory = func() modelRegistrar { return registrar }
	defer func() { modelRegistrarFactory = restoreReg }()

	warnings := pullGraphifyModelIfAbsent(&output.Emitter{JSON: true}, "qwen2.5-coder:7b")
	if len(warnings) != 0 {
		test.Fatalf("warnings = %v, want none", warnings)
	}
	if len(fake.PulledNames) != 1 || fake.PulledNames[0] != "qwen2.5-coder:7b" {
		test.Fatalf("Pulled = %v, want [qwen2.5-coder:7b]", fake.PulledNames)
	}
	if len(registrar.registered) != 1 || registrar.registered[0] != "qwen2.5-coder:7b" {
		test.Fatalf("registered = %v, want [qwen2.5-coder:7b]", registrar.registered)
	}
}

// fakeModelRegistrar records gateway registrations for the pull test.
type fakeModelRegistrar struct {
	registered []string
}

func (fake *fakeModelRegistrar) RegisterOllamaModel(name string) error {
	fake.registered = append(fake.registered, name)
	return nil
}
func (fake *fakeModelRegistrar) UnregisterOllamaModel(string) error { return nil }
