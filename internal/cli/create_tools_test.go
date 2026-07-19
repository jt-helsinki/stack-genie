package cli

import (
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/create"
)

// The unified --tools multi-select replaces the old --caveman/--code-review-graph/
// --codebase-memory bool flags. These assert the flag → per-tool spec bool mapping.

func TestSpecFromFlagsDefaultsAITools(test *testing.T) {
	// --tools unset → the default set (caveman + graphify + code-review-graph on,
	// codebase-memory off).
	spec, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", defaultName: "demo"})
	if err != nil {
		test.Fatalf("specFromFlags: %v", err)
	}
	if !spec.CavemanEnabled || !spec.GraphifyEnabled || !spec.CodeReviewGraphEnabled {
		test.Errorf("default tools: caveman/graphify/code-review-graph must be on, got %+v",
			[]bool{spec.CavemanEnabled, spec.GraphifyEnabled, spec.CodeReviewGraphEnabled})
	}
	if spec.CodebaseMemoryEnabled {
		test.Error("default tools: codebase-memory-mcp must be off")
	}
}

func TestSpecFromFlagsExplicitAITools(test *testing.T) {
	// An explicit subset selects exactly those tools.
	spec, err := specFromFlags(createFlags{
		name: "demo", osKey: "ubuntu", defaultName: "demo",
		tools:    []string{create.AIToolGraphify, create.AIToolCodebaseMemory},
		toolsSet: true,
	})
	if err != nil {
		test.Fatalf("specFromFlags: %v", err)
	}
	if spec.CavemanEnabled || spec.CodeReviewGraphEnabled {
		test.Error("unselected tools (caveman/code-review-graph) must be off")
	}
	if !spec.GraphifyEnabled || !spec.CodebaseMemoryEnabled {
		test.Error("selected tools (graphify/codebase-memory) must be on")
	}
}

func TestSpecFromFlagsEmptyAIToolsSelectsNone(test *testing.T) {
	// --tools="" (provided but empty) selects NO tools.
	spec, err := specFromFlags(createFlags{
		name: "demo", osKey: "ubuntu", defaultName: "demo",
		tools: nil, toolsSet: true,
	})
	if err != nil {
		test.Fatalf("specFromFlags: %v", err)
	}
	if spec.CavemanEnabled || spec.GraphifyEnabled || spec.CodeReviewGraphEnabled || spec.CodebaseMemoryEnabled {
		test.Errorf("--tools=\"\" must select no tools, got %+v", spec)
	}
}

func TestSpecFromFlagsRejectsUnknownAITool(test *testing.T) {
	_, err := specFromFlags(createFlags{
		name: "demo", osKey: "ubuntu", defaultName: "demo",
		tools: []string{"not-a-tool"}, toolsSet: true,
	})
	if err == nil {
		test.Fatal("an unknown --tools value must be rejected")
	}
}
