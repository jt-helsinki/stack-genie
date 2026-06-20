package console

import "testing"

func TestURLAndKnown(test *testing.T) {
	if url, ok := URL("litellm"); !ok || url != "http://localhost:4000/ui" {
		test.Errorf("litellm console = (%q,%v)", url, ok)
	}
	// Known service with no web console.
	if _, ok := URL("ollama"); ok {
		test.Error("ollama should report no console")
	}
	if !Known("ollama") {
		test.Error("ollama should be a known service")
	}
	if Known("nope") {
		test.Error("unknown service must not be Known")
	}
}

func TestWithConsolesSortedAndFiltered(test *testing.T) {
	named := WithConsoles()
	if len(named) != 2 || named[0].Name != "clawpatrol" || named[1].Name != "litellm" {
		test.Fatalf("WithConsoles = %+v, want sorted [clawpatrol, litellm]", named)
	}
}

func TestOpenerCommandPerOS(test *testing.T) {
	cases := map[string]struct {
		name string
		args []string
	}{
		"darwin": {"open", []string{"https://x"}},
		"linux":  {"xdg-open", []string{"https://x"}},
	}
	for goos, want := range cases {
		name, args := osOpener{goos: goos}.command("https://x")
		if name != want.name || len(args) != len(want.args) {
			test.Errorf("%s command = (%q,%v), want (%q,%v)", goos, name, args, want.name, want.args)
			continue
		}
		for index := range args {
			if args[index] != want.args[index] {
				test.Errorf("%s args[%d] = %q, want %q", goos, index, args[index], want.args[index])
			}
		}
	}
}
