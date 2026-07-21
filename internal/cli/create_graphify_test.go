package cli

import (
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/ollama"
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
