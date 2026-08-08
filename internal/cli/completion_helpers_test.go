package cli

import (
	"slices"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// The shell-completion helpers are pure lookups: they never set an exit code, so
// these tests assert the returned candidate set and the NoFileComp directive.

func TestCompleteProjectNames(test *testing.T) {
	seedProjectAt(test, "app")
	names, directive := completeProjectNames(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		test.Fatalf("directive = %v, want NoFileComp", directive)
	}
	if !slices.Contains(names, "app") {
		test.Fatalf("project names %v missing seeded 'app'", names)
	}
}

func TestCompleteProjectArg(test *testing.T) {
	seedProjectAt(test, "app")
	// First positional offers project names.
	names, _ := completeProjectArg(&cobra.Command{}, nil, "")
	if !slices.Contains(names, "app") {
		test.Fatalf("first-position names %v missing 'app'", names)
	}
	// A second positional offers nothing (only one project arg is accepted).
	rest, directive := completeProjectArg(&cobra.Command{}, []string{"app"}, "")
	if len(rest) != 0 || directive != cobra.ShellCompDirectiveNoFileComp {
		test.Fatalf("second-position completion = (%v,%v), want ([],NoFileComp)", rest, directive)
	}
}

func TestCompleteAgentArg(test *testing.T) {
	seedProjectAt(test, "app")
	// Position 0 offers the agent CLI names.
	clis, _ := completeAgentArg(&cobra.Command{}, nil, "")
	if !slices.Contains(clis, "opencode") {
		test.Fatalf("agent CLI names %v missing 'opencode'", clis)
	}
	// Position 1 offers project names.
	names, _ := completeAgentArg(&cobra.Command{}, []string{"opencode"}, "")
	if !slices.Contains(names, "app") {
		test.Fatalf("agent second-position names %v missing 'app'", names)
	}
	// Position 2+ offers nothing.
	rest, _ := completeAgentArg(&cobra.Command{}, []string{"opencode", "app"}, "")
	if len(rest) != 0 {
		test.Fatalf("agent third-position completion = %v, want empty", rest)
	}
}

func TestCompleteServiceNames(test *testing.T) {
	names, directive := completeServiceNames(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		test.Fatalf("directive = %v, want NoFileComp", directive)
	}
	if !slices.Contains(names, "all") {
		test.Fatalf("service names %v missing the 'all' sentinel", names)
	}
	if !slices.Contains(names, "vllm") {
		test.Fatalf("service names %v missing a known service (vllm)", names)
	}
}

func TestCompleteOptionalServiceNames(test *testing.T) {
	// The optional-service set may be empty (the mechanism is retained but unused),
	// so only the directive is guaranteed.
	_, directive := completeOptionalServiceNames(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		test.Fatalf("directive = %v, want NoFileComp", directive)
	}
}

func TestCompleteConsoleServices(test *testing.T) {
	_, directive := completeConsoleServices(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		test.Fatalf("directive = %v, want NoFileComp", directive)
	}
}

func TestFixedValues(test *testing.T) {
	values, directive := fixedValues("a", "b")(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		test.Fatalf("directive = %v, want NoFileComp", directive)
	}
	if !slices.Equal(values, []string{"a", "b"}) {
		test.Fatalf("fixedValues = %v, want [a b]", values)
	}
}

func TestCompleteOptionalProjectThenValue(test *testing.T) {
	seedProjectAt(test, "app")
	completer := completeOptionalProjectThenValue([]string{"low", "high"})

	// Position 0: enum values AND project names (either may come first).
	first, _ := completer(&cobra.Command{}, nil, "")
	if !slices.Contains(first, "low") || !slices.Contains(first, "app") {
		test.Fatalf("position-0 completion %v should have values + project names", first)
	}
	// Position 1: only the enum values.
	second, _ := completer(&cobra.Command{}, []string{"app"}, "")
	if !slices.Equal(second, []string{"low", "high"}) {
		test.Fatalf("position-1 completion = %v, want [low high]", second)
	}
	// Position 2+: nothing.
	third, _ := completer(&cobra.Command{}, []string{"app", "low"}, "")
	if len(third) != 0 {
		test.Fatalf("position-2 completion = %v, want empty", third)
	}
}

func TestCompleteThemeNames(test *testing.T) {
	names, directive := completeThemeNames(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		test.Fatalf("directive = %v, want NoFileComp", directive)
	}
	if !slices.Equal(names, ui.ThemeNames()) {
		test.Fatalf("theme completion = %v, want %v", names, ui.ThemeNames())
	}
}
