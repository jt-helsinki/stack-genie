package cli

import (
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

func TestMergeModelsInstalledFirstAndDedup(test *testing.T) {
	installed := []ollama.Model{
		{Name: "gemma4:31b", Size: 100, ParameterSize: "31B"},
		{Name: "custom-thing:latest", Size: 50, ParameterSize: "7B"},
	}
	merged := mergeModels(installed, ollama.Catalog())

	// Installed-but-not-in-catalog model still appears, marked installed.
	var sawCustom, sawGemmaInstalled, sawLlamaAvailable bool
	for _, entry := range merged {
		switch {
		case entry.Name == "custom-thing:latest":
			sawCustom = entry.Installed
		case entry.Name == "gemma4:31b":
			sawGemmaInstalled = entry.Installed
		case entry.Name == "llama3.2":
			sawLlamaAvailable = !entry.Installed
		case entry.Name == "gemma4":
			test.Fatal("catalog gemma4 must be hidden — gemma4:31b is installed (base-name match)")
		}
	}
	if !sawCustom {
		test.Fatal("installed-not-in-catalog model missing or not marked installed")
	}
	if !sawGemmaInstalled {
		test.Fatal("gemma4:31b should be installed")
	}
	if !sawLlamaAvailable {
		test.Fatal("a catalog model (llama3.2) should appear as available")
	}
	// Installed entries sort before available entries.
	if merged[0].Installed != true {
		test.Fatalf("first entry should be installed, got %+v", merged[0])
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
	result := modelsListResult{Models: mergeModels(
		[]ollama.Model{{Name: "gemma4:31b", Size: 1610612736, ParameterSize: "31B"}},
		ollama.Catalog(),
	)}
	human := result.Human()
	for _, want := range []string{"NAME", "STATUS", "installed", "available", "1.5 GB"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

// ollamaUnreachable returns an error classified as Ollama-down for tests.
func ollamaUnreachable() error { return ollama.NewUnreachable() }
