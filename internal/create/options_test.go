package create

import (
	"slices"
	"testing"
)

// TestSplitAITools verifies each tool key toggles ONLY its own bool — a wrong mapping
// mis-provisions every workspace's per-project tooling.
func TestSplitAITools(test *testing.T) {
	cases := []struct {
		name                                            string
		tools                                           []string
		caveman, graphify, codeReviewGraph, codebaseMem bool
	}{
		{name: "empty is all false"},
		{name: "caveman only", tools: []string{AIToolCaveman}, caveman: true},
		{name: "graphify only", tools: []string{AIToolGraphify}, graphify: true},
		{name: "code-review-graph only", tools: []string{AIToolCodeReviewGraph}, codeReviewGraph: true},
		{name: "codebase-memory only", tools: []string{AIToolCodebaseMemory}, codebaseMem: true},
		{
			name:            "all four",
			tools:           []string{AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph, AIToolCodebaseMemory},
			caveman:         true,
			graphify:        true,
			codeReviewGraph: true,
			codebaseMem:     true,
		},
		{name: "unknown key ignored", tools: []string{"bogus-tool"}},
		{
			name:     "unknown key alongside a real one",
			tools:    []string{"bogus-tool", AIToolGraphify},
			graphify: true,
		},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			caveman, graphify, codeReviewGraph, codebaseMem := SplitAITools(testCase.tools)
			if caveman != testCase.caveman {
				test.Errorf("caveman = %v, want %v", caveman, testCase.caveman)
			}
			if graphify != testCase.graphify {
				test.Errorf("graphify = %v, want %v", graphify, testCase.graphify)
			}
			if codeReviewGraph != testCase.codeReviewGraph {
				test.Errorf("codeReviewGraph = %v, want %v", codeReviewGraph, testCase.codeReviewGraph)
			}
			if codebaseMem != testCase.codebaseMem {
				test.Errorf("codebaseMemory = %v, want %v", codebaseMem, testCase.codebaseMem)
			}
		})
	}
}

// TestDefaultAITools asserts the on-by-default set is exactly caveman + graphify +
// code-review-graph (codebase-memory is off by default).
func TestDefaultAITools(test *testing.T) {
	got := DefaultAITools()
	want := []string{AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph}
	if !slices.Equal(got, want) {
		test.Fatalf("DefaultAITools() = %v, want %v", got, want)
	}
	if slices.Contains(got, AIToolCodebaseMemory) {
		test.Errorf("codebase-memory must be OFF by default; got %v", got)
	}
}

// TestSupportedAITools asserts all four tool keys are offered.
func TestSupportedAITools(test *testing.T) {
	got := SupportedAITools()
	for _, key := range []string{AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph, AIToolCodebaseMemory} {
		if !slices.Contains(got, key) {
			test.Errorf("SupportedAITools() = %v, missing %q", got, key)
		}
	}
	if len(got) != 4 {
		test.Errorf("SupportedAITools() should offer exactly 4 keys, got %d: %v", len(got), got)
	}
}

// TestSupportedShells asserts bash + zsh are the shell choices (bash the platform default).
func TestSupportedShells(test *testing.T) {
	got := SupportedShells()
	if !slices.Equal(got, []string{"bash", "zsh"}) {
		test.Fatalf("SupportedShells() = %v, want [bash zsh]", got)
	}
}

// TestSupportedAuthModes asserts the per-agent auth modes are api-key + oauth.
func TestSupportedAuthModes(test *testing.T) {
	got := SupportedAuthModes()
	if !slices.Equal(got, []string{"api-key", "oauth"}) {
		test.Fatalf("SupportedAuthModes() = %v, want [api-key oauth]", got)
	}
}

// TestOAuthCapableCLIs asserts the subscription-login CLIs (claude-code, codex, gemini)
// are the ones that can be set to oauth, and no others leak in.
func TestOAuthCapableCLIs(test *testing.T) {
	got := OAuthCapableCLIs()
	for _, cli := range []string{"claude-code", "codex", "gemini"} {
		if !slices.Contains(got, cli) {
			test.Errorf("OAuthCapableCLIs() = %v, missing %q", got, cli)
		}
	}
	for _, notCapable := range []string{"opencode", "omp", "copilot", "hermes"} {
		if slices.Contains(got, notCapable) {
			test.Errorf("OAuthCapableCLIs() must not contain %q; got %v", notCapable, got)
		}
	}
}

// TestHostResourceWrappers checks the sysinfo wrappers return sane, non-negative values
// and that the usable memory never exceeds total host memory.
func TestHostResourceWrappers(test *testing.T) {
	if cpus := HostCPUs(); cpus < 0 {
		test.Errorf("HostCPUs() = %d, want non-negative", cpus)
	}
	hostGB := HostMemoryGB()
	if hostGB < 0 {
		test.Errorf("HostMemoryGB() = %d, want non-negative", hostGB)
	}
	usableGB := UsableHostMemoryGB()
	if usableGB < 0 {
		test.Errorf("UsableHostMemoryGB() = %d, want non-negative", usableGB)
	}
	if usableGB > hostGB {
		test.Errorf("UsableHostMemoryGB() = %d must be <= HostMemoryGB() = %d", usableGB, hostGB)
	}
}
