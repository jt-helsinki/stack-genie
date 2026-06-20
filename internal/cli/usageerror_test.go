package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestHumanizeUsageError(test *testing.T) {
	cases := map[string]string{
		"accepts 1 arg(s), received 0":                "needs 1 argument(s), received 0",
		"requires at least 1 arg(s), only received 0": "needs at least 1 argument(s), received 0",
		"accepts at most 1 arg(s), received 3":        "needs at most 1 argument(s), received 3",
		`unknown command "bogus" for "ai"`:            `unknown command "bogus" for "ai"`, // passes through
		"unknown flag: --nope":                        "unknown flag: --nope",             // passes through
	}
	for input, want := range cases {
		if got := humanizeUsageError(input); got != want {
			test.Errorf("humanizeUsageError(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUsageCommandName(test *testing.T) {
	root := &cobra.Command{Use: "ai"}
	project := &cobra.Command{Use: "project"}
	create := &cobra.Command{Use: "create [name]"}
	project.AddCommand(create)
	root.AddCommand(project)

	cases := map[*cobra.Command]string{
		nil:     "ai",
		root:    "ai",
		project: "project",
		create:  "project.create",
	}
	for cmd, want := range cases {
		if got := usageCommandName(cmd); got != want {
			test.Errorf("usageCommandName = %q, want %q", got, want)
		}
	}
}
