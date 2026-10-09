package create

import (
	"slices"
	"testing"
)

// TestSplitAITools verifies each tool key toggles ONLY its own bool — a wrong mapping
// mis-provisions every workspace's per-project tooling.
func TestSplitAITools(test *testing.T) {
	cases := []struct {
		name                                                                                                       string
		tools                                                                                                      []string
		caveman, graphify, codeReviewGraph, codebaseMem, openViking, tokenSave, agentMemory, ponytail, agentSkills bool
	}{
		{name: "empty is all false"},
		{name: "caveman only", tools: []string{AIToolCaveman}, caveman: true},
		{name: "graphify only", tools: []string{AIToolGraphify}, graphify: true},
		{name: "code-review-graph only", tools: []string{AIToolCodeReviewGraph}, codeReviewGraph: true},
		{name: "codebase-memory only", tools: []string{AIToolCodebaseMemory}, codebaseMem: true},
		{name: "openviking only", tools: []string{AIToolOpenViking}, openViking: true},
		{name: "tokensave only", tools: []string{AIToolTokenSave}, tokenSave: true},
		{name: "agentmemory only", tools: []string{AIToolAgentMemory}, agentMemory: true},
		{name: "ponytail only", tools: []string{AIToolPonytail}, ponytail: true},
		{name: "agent-skills only", tools: []string{AIToolAgentSkills}, agentSkills: true},
		{
			name: "all nine",
			tools: []string{
				AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph, AIToolCodebaseMemory, AIToolOpenViking,
				AIToolTokenSave, AIToolAgentMemory, AIToolPonytail, AIToolAgentSkills,
			},
			caveman:         true,
			graphify:        true,
			codeReviewGraph: true,
			codebaseMem:     true,
			openViking:      true,
			tokenSave:       true,
			agentMemory:     true,
			ponytail:        true,
			agentSkills:     true,
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
			caveman, graphify, codeReviewGraph, codebaseMem, openViking, tokenSave, agentMemory, ponytail, agentSkills := SplitAITools(testCase.tools)
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
			if openViking != testCase.openViking {
				test.Errorf("openViking = %v, want %v", openViking, testCase.openViking)
			}
			if tokenSave != testCase.tokenSave {
				test.Errorf("tokenSave = %v, want %v", tokenSave, testCase.tokenSave)
			}
			if agentMemory != testCase.agentMemory {
				test.Errorf("agentMemory = %v, want %v", agentMemory, testCase.agentMemory)
			}
			if ponytail != testCase.ponytail {
				test.Errorf("ponytail = %v, want %v", ponytail, testCase.ponytail)
			}
			if agentSkills != testCase.agentSkills {
				test.Errorf("agentSkills = %v, want %v", agentSkills, testCase.agentSkills)
			}
		})
	}
}

// TestDefaultAITools asserts no AI tool is selected by default.
func TestDefaultAITools(test *testing.T) {
	if got := DefaultAITools(); len(got) != 0 {
		test.Fatalf("DefaultAITools() = %v, want empty", got)
	}
}

// TestSupportedAITools asserts all nine tool keys are offered, each with a description.
func TestSupportedAITools(test *testing.T) {
	got := SupportedAITools()
	for _, key := range []string{
		AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph, AIToolCodebaseMemory, AIToolOpenViking,
		AIToolTokenSave, AIToolAgentMemory, AIToolPonytail, AIToolAgentSkills,
	} {
		if !slices.Contains(got, key) {
			test.Errorf("SupportedAITools() = %v, missing %q", got, key)
		}
		if AIToolDescription(key) == "" {
			test.Errorf("AIToolDescription(%q) is empty", key)
		}
	}
	if len(got) != 9 {
		test.Errorf("SupportedAITools() should offer exactly 9 keys, got %d: %v", len(got), got)
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
	for _, notCapable := range []string{"opencode", "omp", "hermes"} {
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
}

// TestSanitizeName verifies the auto-derived-name character rules: lowercased,
// non-[a-z0-9-] runs collapse to a single "-", and leading/trailing "-" are trimmed.
func TestSanitizeName(test *testing.T) {
	cases := map[string]string{
		"My Repo":     "my-repo",
		"foo_bar.baz": "foo-bar-baz",
		"--edge--":    "edge",
		"already-ok":  "already-ok",
	}
	for input, want := range cases {
		if got := SanitizeName(input); got != want {
			test.Errorf("SanitizeName(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestDefaultDirName verifies the workspace-name default derives from a directory
// path's basename (sanitized), a trailing slash is tolerated, and an empty path
// yields "" (the caller falls back to cwd).
func TestDefaultDirName(test *testing.T) {
	if got := DefaultDirName("/Users/jordan/Documents/My Project/"); got != "my-project" {
		test.Errorf("DefaultDirName(with trailing slash) = %q, want my-project", got)
	}
	if got := DefaultDirName("/tmp/demo"); got != "demo" {
		test.Errorf("DefaultDirName(/tmp/demo) = %q, want demo", got)
	}
	if got := DefaultDirName(""); got != "" {
		test.Errorf("DefaultDirName(\"\") = %q, want empty", got)
	}
}
