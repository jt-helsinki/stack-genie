package logs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/state"
)

func TestTail(test *testing.T) {
	dir := test.TempDir()
	write := func(name, content string) string {
		test.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			test.Fatal(err)
		}
		return path
	}

	cases := []struct {
		name    string
		content string
		lines   int
		want    []string
	}{
		{name: "keeps last N of a longer file", content: "a\nb\nc\nd\n", lines: 2, want: []string{"c", "d"}},
		{name: "shorter than N keeps everything", content: "a\nb\n", lines: 10, want: []string{"a", "b"}},
		{name: "zero keeps everything", content: "a\nb\nc\n", lines: 0, want: []string{"a", "b", "c"}},
		{name: "negative keeps everything", content: "a\nb\n", lines: -1, want: []string{"a", "b"}},
		{name: "no trailing newline", content: "a\nb", lines: 1, want: []string{"b"}},
		{name: "empty file yields empty slice", content: "", lines: 5, want: []string{}},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			got, err := Tail(write("f.log", tc.content), tc.lines)
			if err != nil {
				test.Fatalf("Tail: %v", err)
			}
			if len(got) != len(tc.want) {
				test.Fatalf("Tail = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					test.Fatalf("Tail = %q, want %q", got, tc.want)
				}
			}
		})
	}

	test.Run("missing file is an error", func(test *testing.T) {
		if _, err := Tail(filepath.Join(dir, "absent.log"), 5); err == nil {
			test.Error("Tail on a missing file should error")
		}
	})
}

func TestSourcesFiltersPlatformLogs(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	logsDir := filepath.Join(home, ".ai-platform", "logs")
	if err := os.MkdirAll(filepath.Join(logsDir, "subdir"), 0o755); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"aip-litellm.log", "aip-ollama.log", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(logsDir, name), []byte("x\n"), 0o644); err != nil {
			test.Fatal(err)
		}
	}

	// No filter: every *.log (and only *.log — no dirs, no .txt).
	all, err := Sources("", "")
	if err != nil {
		test.Fatalf("Sources: %v", err)
	}
	if len(all) != 2 {
		test.Fatalf("Sources = %v, want the two .log files", all)
	}
	// Service filter is a substring match on the file name.
	litellm, err := Sources("", "litellm")
	if err != nil {
		test.Fatalf("Sources(litellm): %v", err)
	}
	if len(litellm) != 1 || !strings.HasSuffix(litellm[0], "aip-litellm.log") {
		test.Fatalf("Sources(litellm) = %v, want just aip-litellm.log", litellm)
	}
}

func TestSourcesIncludesWorkspaceRunDir(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	root := filepath.Join(home, "projects", "my-app")
	runDir := filepath.Join(root, ".ai-platform", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "start.log"), []byte("boot\n"), 0o644); err != nil {
		test.Fatal(err)
	}
	index := state.NewProjectsIndex()
	index.Projects["my-app"] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}

	// The platform logs dir does not exist — that is fine (no error), and the
	// project run/ dir still contributes its logs.
	sources, err := Sources("my-app", "")
	if err != nil {
		test.Fatalf("Sources: %v", err)
	}
	if len(sources) != 1 || !strings.HasSuffix(sources[0], filepath.Join("run", "start.log")) {
		test.Fatalf("Sources = %v, want just the run/start.log", sources)
	}
}

func TestSourcesUnknownWorkspaceErrors(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if _, err := Sources("nope", ""); !errors.Is(err, project.ErrUnknownProject) {
		test.Errorf("Sources(unknown) err = %v, want ErrUnknownProject", err)
	}
}

func TestSourcesMissingDirsYieldEmpty(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	sources, err := Sources("", "")
	if err != nil {
		test.Fatalf("Sources: %v", err)
	}
	if len(sources) != 0 {
		test.Errorf("Sources with no logs dir = %v, want empty", sources)
	}
}

func TestServicesIsAFreshCopy(test *testing.T) {
	first := Services()
	if len(first) == 0 {
		test.Fatal("Services() should list the log scopes")
	}
	first[0] = "mutated"
	if second := Services(); second[0] == "mutated" {
		test.Error("Services() must return a fresh copy, not the backing slice")
	}
}
