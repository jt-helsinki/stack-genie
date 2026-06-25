package cli

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// withFakeOllama swaps the package-level ollamaClient constructor for one that
// returns the given fake, restoring it after the test.
func withFakeOllama(test *testing.T, fake *ollama.Fake) {
	test.Helper()
	prev := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	test.Cleanup(func() { ollamaClient = prev })
}

func runLocalModelsCmd(test *testing.T, cmd interface {
	SetArgs([]string)
	Execute() error
}, args ...string) {
	test.Helper()
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("command %v returned error: %v", args, err)
	}
}

func jsonEmitter() *output.Emitter {
	return &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
}

func TestInstalledEntriesListsInstalledOnly(test *testing.T) {
	installed := []ollama.Model{
		{Name: "gemma4:31b", Size: 100, ParameterSize: "31B"},
		{Name: "custom-thing:latest", Size: 50, ParameterSize: "7B"},
	}
	entries := installedEntries(installed)
	if len(entries) != 2 {
		test.Fatalf("got %d entries, want 2 (installed-only, no catalog)", len(entries))
	}
	for _, entry := range entries {
		if !entry.Installed {
			test.Fatalf("entry %q should be marked installed", entry.Name)
		}
	}
	if entries[0].Name != "gemma4:31b" || entries[0].Size != 100 || entries[0].Params != "31B" {
		test.Fatalf("first entry = %+v", entries[0])
	}
}

func TestModelsListUnreachableExits3(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{ListErr: ollamaUnreachable()})
	exit := output.ExitOK
	cmd := newModelsListCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitMissingDep {
		test.Fatalf("list with unreachable Ollama: exit = %d, want %d", exit, output.ExitMissingDep)
	}
}

func TestModelsPullDirectName(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b")
	if exit != output.ExitOK {
		test.Fatalf("pull exit = %d, want 0", exit)
	}
	if fake.PulledName != "llama3.2:3b" {
		test.Fatalf("pulled %q, want llama3.2:3b", fake.PulledName)
	}
}

func TestModelsPullMissingNameNonInteractiveExits2(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitInvalidInput {
		test.Fatalf("pull with no name under --json: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestModelsRmDirectName(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	exit := output.ExitOK
	// JSON emitter => no confirm prompt.
	cmd := newModelsRmCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b")
	if exit != output.ExitOK {
		test.Fatalf("rm exit = %d, want 0", exit)
	}
	if fake.RemovedName != "llama3.2:3b" {
		test.Fatalf("removed %q, want llama3.2:3b", fake.RemovedName)
	}
}

func TestModelsRmNotFoundExits2(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{RemoveErr: &ollama.NotFoundError{Name: "ghost"}})
	exit := output.ExitOK
	cmd := newModelsRmCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "ghost")
	if exit != output.ExitInvalidInput {
		test.Fatalf("rm not-found: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestModelsShowDirectName(test *testing.T) {
	fake := &ollama.Fake{ShowInfo: ollama.ModelInfo{Name: "gemma4", ParameterSize: "31B"}}
	withFakeOllama(test, fake)
	exit := output.ExitOK
	cmd := newModelsShowCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "gemma4")
	if exit != output.ExitOK {
		test.Fatalf("show exit = %d, want 0", exit)
	}
	if fake.ShownName != "gemma4" {
		test.Fatalf("shown %q, want gemma4", fake.ShownName)
	}
}

func TestModelsListHumanTable(test *testing.T) {
	result := modelsListResult{Models: installedEntries(
		[]ollama.Model{{Name: "gemma4:31b", Size: 1610612736, ParameterSize: "31B"}},
	)}
	human := result.Human()
	for _, want := range []string{"NAME", "SIZE", "PARAMS", "gemma4:31b", "1.5 GB"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

func TestModelsPopularHumanTableAndJSON(test *testing.T) {
	result := modelsPopularResult{Models: toPopularEntries([]ollama.PopularModel{
		{Name: "gemma4", Parameters: []string{"e2b", "31b"}, DownloadSize: 1610612736, RepoURL: "https://ollama.com/library/gemma4"},
		{Name: "glm-5.2", DownloadSize: 0, RepoURL: "https://ollama.com/library/glm-5.2"},
	})}
	human := result.Human()
	for _, want := range []string{"NAME", "PARAMS", "SIZE", "REPO", "gemma4", "e2b,31b", "1.5 GB", "ollama.com/library/gemma4", "—"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

// withFakePopular swaps the package-level ollamaPopular fetcher for a stub,
// restoring it after the test (no network).
func withFakePopular(test *testing.T, models []ollama.PopularModel, err error) {
	test.Helper()
	prev := ollamaPopular
	ollamaPopular = func() ([]ollama.PopularModel, error) { return models, err }
	test.Cleanup(func() { ollamaPopular = prev })
}

func TestModelsPopularSuccess(test *testing.T) {
	withFakePopular(test, []ollama.PopularModel{
		{Name: "gemma4", Parameters: []string{"31b"}, DownloadSize: 100, RepoURL: "https://ollama.com/library/gemma4"},
	}, nil)
	exit := output.ExitOK
	cmd := newModelsPopularCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitOK {
		test.Fatalf("popular exit = %d, want 0", exit)
	}
}

func TestModelsPopularFetchFailureExits4(test *testing.T) {
	withFakePopular(test, nil, errors.New("no internet"))
	exit := output.ExitOK
	cmd := newModelsPopularCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("popular fetch failure exit = %d, want %d", exit, output.ExitRuntimeFailure)
	}
}

// ollamaUnreachable returns an error classified as Ollama-down for tests.
func ollamaUnreachable() error { return ollama.NewUnreachable() }
