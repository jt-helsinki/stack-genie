package cli

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

// withFakeOllama swaps the package-level ollamaClient constructor for one that
// returns the given fake, restoring it after the test.
func withFakeOllama(test *testing.T, fake *ollama.Fake) {
	test.Helper()
	prev := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	test.Cleanup(func() { ollamaClient = prev })
}

// fakeRegistrar records the model names register/unregister were called with and can
// be configured to fail (to prove pull/rm tolerate a gateway error).
type fakeRegistrar struct {
	registered   []string
	unregistered []string
	registerErr  error
	unregErr     error
}

func (fake *fakeRegistrar) RegisterOllamaModel(name string, _ bool) error {
	fake.registered = append(fake.registered, name)
	return fake.registerErr
}

func (fake *fakeRegistrar) UnregisterOllamaModel(name string) error {
	fake.unregistered = append(fake.unregistered, name)
	return fake.unregErr
}

// withFakeRegistrar swaps the package-level modelRegistrarFactory for one returning
// the given fake (no network), restoring it after the test. It also redirects HOME to
// a temp dir so the best-effort runtime-choice store writes (config.SetModelRuntime)
// land in an isolated location, not the developer's real ~/.ai-platform.
func withFakeRegistrar(test *testing.T, fake *fakeRegistrar) {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	prev := modelRegistrarFactory
	modelRegistrarFactory = func() modelRegistrar { return fake }
	test.Cleanup(func() { modelRegistrarFactory = prev })
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
	withFakeRegistrar(test, &fakeRegistrar{})
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

// Passing several names pulls each in turn (in order) and reports a per-model result.
func TestModelsPullMultipleNames(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	withFakeRegistrar(test, &fakeRegistrar{})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b", "qwen2.5:7b", "gemma4:2b")
	if exit != output.ExitOK {
		test.Fatalf("multi pull exit = %d, want 0", exit)
	}
	want := []string{"llama3.2:3b", "qwen2.5:7b", "gemma4:2b"}
	if strings.Join(fake.PulledNames, ",") != strings.Join(want, ",") {
		test.Fatalf("pulled %v, want %v", fake.PulledNames, want)
	}
}

// Duplicate references are pulled once (de-duplicated, first-seen order preserved).
func TestModelsPullDedupesNames(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	withFakeRegistrar(test, &fakeRegistrar{})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b", "llama3.2:3b", " qwen2.5:7b ", "")
	if exit != output.ExitOK {
		test.Fatalf("pull exit = %d, want 0", exit)
	}
	want := []string{"llama3.2:3b", "qwen2.5:7b"}
	if strings.Join(fake.PulledNames, ",") != strings.Join(want, ",") {
		test.Fatalf("pulled %v, want %v (deduped, trimmed)", fake.PulledNames, want)
	}
}

// The run continues past a failing model and exits non-zero (mapped from the failure),
// while still pulling the remaining models and returning a per-model result list.
func TestModelsPullContinuesPastFailure(test *testing.T) {
	fake := &ollama.Fake{
		PullErrs: map[string]error{"bad-model": &ollama.NotFoundError{Name: "bad-model"}},
	}
	withFakeOllama(test, fake)
	withFakeRegistrar(test, &fakeRegistrar{})
	exit := output.ExitOK
	emitter := jsonEmitter()
	cmd := newModelsPullCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "good-a", "bad-model", "good-b")
	// Every model was attempted, in order, despite the middle one failing.
	want := []string{"good-a", "bad-model", "good-b"}
	if strings.Join(fake.PulledNames, ",") != strings.Join(want, ",") {
		test.Fatalf("attempted %v, want %v (run continues past failure)", fake.PulledNames, want)
	}
	// A not-found model maps to exit 2 (invalid input).
	if exit != output.ExitInvalidInput {
		test.Fatalf("pull-with-failure exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// The pull result Human() renders a per-model line with the SPECIFIC ref, marking
// each success/failure.
func TestModelsPullResultHuman(test *testing.T) {
	result := modelsPullResult{Pulled: []modelPullOutcome{
		{Model: "llama3.2:3b", OK: true},
		{Model: "bad-model", OK: false, Error: "not found"},
	}}
	human := result.Human()
	for _, want := range []string{"llama3.2:3b", "bad-model", "not found"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
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
	withFakeRegistrar(test, &fakeRegistrar{})
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

// libTags builds library tags from short names (test convenience).
func libTags(names ...string) []ollama.LibraryTag {
	tags := make([]ollama.LibraryTag, len(names))
	for index, name := range names {
		tags[index] = ollama.LibraryTag{Name: name}
	}
	return tags
}

func TestModelsPopularHumanTableAndJSON(test *testing.T) {
	result := modelsPopularResult{Models: toPopularEntries([]ollama.LibraryModel{
		{Name: "gemma4", Description: "Google Gemma", RepoURL: "https://ollama.com/library/gemma4", Tags: []ollama.LibraryTag{
			{Name: "latest", Size: "3.3GB", Context: "128K", Input: "Text"},
			{Name: "4b", Size: "3.3GB", Context: "128K", Input: "Text"},
		}},
		{Name: "nomic-embed-text", Description: "An embedding model", Tags: nil, RepoURL: "https://ollama.com/library/nomic-embed-text"},
	})}
	human := result.Human()
	// Columns: NAME / SIZE / CONTEXT / INPUT / REPO. The representative tag's
	// values are shown; a model with no tags shows dashes.
	for _, want := range []string{"NAME", "SIZE", "CONTEXT", "INPUT", "REPO", "gemma4", "3.3GB", "128K", "Text", "ollama.com/library/gemma4", "—"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

// withFakeLibrary swaps the package-level ollamaLibrary loader for a stub,
// restoring it after the test (no network).
func withFakeLibrary(test *testing.T, models []ollama.LibraryModel, source ollama.Source, err error) {
	test.Helper()
	prev := ollamaLibrary
	ollamaLibrary = func() ([]ollama.LibraryModel, ollama.Source, error) { return models, source, err }
	test.Cleanup(func() { ollamaLibrary = prev })
}

func TestModelsPopularSuccess(test *testing.T) {
	withFakeLibrary(test, []ollama.LibraryModel{
		{Name: "gemma4", Tags: libTags("31b"), RepoURL: "https://ollama.com/library/gemma4"},
	}, ollama.SourceFresh, nil)
	exit := output.ExitOK
	cmd := newModelsPopularCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitOK {
		test.Fatalf("popular exit = %d, want 0", exit)
	}
}

// A cached copy (live fetch failed but cache present) still succeeds.
func TestModelsPopularCachedStillSucceeds(test *testing.T) {
	withFakeLibrary(test, []ollama.LibraryModel{
		{Name: "gemma4", Tags: libTags("31b"), RepoURL: "https://ollama.com/library/gemma4"},
	}, ollama.SourceCached, errors.New("no internet"))
	exit := output.ExitOK
	cmd := newModelsPopularCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitOK {
		test.Fatalf("popular (cached) exit = %d, want 0", exit)
	}
}

func TestModelsPopularFetchFailureExits4(test *testing.T) {
	withFakeLibrary(test, nil, ollama.SourceCached, errors.New("no internet"))
	exit := output.ExitOK
	cmd := newModelsPopularCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("popular fetch failure exit = %d, want %d", exit, output.ExitRuntimeFailure)
	}
}

func TestLibraryPullRefs(test *testing.T) {
	refs := libraryPullRefs([]ollama.LibraryModel{
		{Name: "qwen2.5", Tags: libTags("7b", "72b")},
		{Name: "nomic-embed-text", Tags: nil},
	})
	want := []string{"qwen2.5:7b", "qwen2.5:72b", "nomic-embed-text"}
	if strings.Join(refs, ",") != strings.Join(want, ",") {
		test.Fatalf("libraryPullRefs = %v, want %v", refs, want)
	}
}

// parseModelRefs splits free text on whitespace/commas and de-duplicates — the
// custom-entry path of the interactive multi-pull.
func TestParseModelRefs(test *testing.T) {
	got := parseModelRefs("llama3.2:1b qwen2.5:7b, llama3.2:1b\thf.co/u/m")
	want := []string{"llama3.2:1b", "qwen2.5:7b", "hf.co/u/m"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		test.Fatalf("parseModelRefs = %v, want %v", got, want)
	}
	if len(parseModelRefs("   ,  ,")) != 0 {
		test.Fatalf("parseModelRefs of separators-only should be empty")
	}
}

// ollamaUnreachable returns an error classified as Ollama-down for tests.
func ollamaUnreachable() error { return ollama.NewUnreachable() }

// A successful pull registers each pulled ref in the gateway (the full ref Ollama
// reports, e.g. llama3.2:3b), and reports registration in the result.
func TestModelsPullRegistersEachRef(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b", "qwen2.5:7b")
	if exit != output.ExitOK {
		test.Fatalf("pull exit = %d, want 0", exit)
	}
	want := []string{"llama3.2:3b", "qwen2.5:7b"}
	if strings.Join(registrar.registered, ",") != strings.Join(want, ",") {
		test.Fatalf("registered %v, want %v", registrar.registered, want)
	}
}

// A gateway registration failure is best-effort: the pull still succeeds (exit 0)
// and the per-model result records the registration error.
func TestModelsPullToleratesRegistrarError(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{registerErr: errors.New("gateway down")}
	withFakeRegistrar(test, registrar)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b")
	if exit != output.ExitOK {
		test.Fatalf("pull with registrar error: exit = %d, want 0 (best-effort)", exit)
	}
	if len(registrar.registered) != 1 || registrar.registered[0] != "llama3.2:3b" {
		test.Fatalf("registered %v, want [llama3.2:3b]", registrar.registered)
	}
}

// The pull-result Human() shows the gateway-registration warning for a model whose
// registration was skipped, while still reporting the pull as successful.
func TestModelsPullResultHumanRegisterWarning(test *testing.T) {
	result := modelsPullResult{Pulled: []modelPullOutcome{
		{Model: "llama3.2:3b", OK: true, RegisterError: "gateway down"},
	}}
	human := result.Human()
	for _, want := range []string{"llama3.2:3b", "gateway down", "registration"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

// A successful rm unregisters the model in the gateway.
func TestModelsRmUnregisters(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	exit := output.ExitOK
	cmd := newModelsRmCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b")
	if exit != output.ExitOK {
		test.Fatalf("rm exit = %d, want 0", exit)
	}
	if len(registrar.unregistered) != 1 || registrar.unregistered[0] != "llama3.2:3b" {
		test.Fatalf("unregistered %v, want [llama3.2:3b]", registrar.unregistered)
	}
}

// A gateway unregistration failure is best-effort: the rm still succeeds (exit 0).
func TestModelsRmToleratesUnregisterError(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{unregErr: errors.New("gateway down")}
	withFakeRegistrar(test, registrar)
	exit := output.ExitOK
	cmd := newModelsRmCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b")
	if exit != output.ExitOK {
		test.Fatalf("rm with unregister error: exit = %d, want 0 (best-effort)", exit)
	}
	if len(registrar.unregistered) != 1 {
		test.Fatalf("unregister should still be attempted, got %v", registrar.unregistered)
	}
}

// A failed Ollama remove must NOT attempt unregistration (the model still exists
// locally).
func TestModelsRmFailureSkipsUnregister(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{RemoveErr: &ollama.NotFoundError{Name: "ghost"}})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	exit := output.ExitOK
	cmd := newModelsRmCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "ghost")
	if exit != output.ExitInvalidInput {
		test.Fatalf("rm not-found: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
	if len(registrar.unregistered) != 0 {
		test.Fatalf("a failed remove should not unregister, got %v", registrar.unregistered)
	}
}

// --- runtime selection -------------------------------------------------------

// The default (no --runtime) registers via Ollama — unchanged behaviour.
func TestModelsPullDefaultRuntimeOllama(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "llama3.2:3b")
	if exit != output.ExitOK {
		test.Fatalf("pull exit = %d, want 0", exit)
	}
	if len(registrar.registered) != 1 || registrar.registered[0] != "llama3.2:3b" {
		test.Fatalf("Ollama registered = %v, want [llama3.2:3b]", registrar.registered)
	}
}

// An invalid --runtime is rejected with exit 2 before any pull.
func TestModelsPullInvalidRuntimeExits2(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	withFakeRegistrar(test, &fakeRegistrar{})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "bogus", "llama3.2:3b")
	if exit != output.ExitInvalidInput {
		test.Fatalf("invalid runtime: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
	if fake.PulledName != "" {
		test.Fatalf("no model should be pulled on an invalid runtime, pulled %q", fake.PulledName)
	}
}
