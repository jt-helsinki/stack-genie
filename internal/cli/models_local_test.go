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
	registered      []string
	unregistered    []string
	dmrRegistered   []string // "<alias>=<model>" per RegisterDockerModelRunnerModel
	dmrUnregistered []string // alias per UnregisterDockerModelRunnerModel
	registerErr     error
	unregErr        error
	dmrRegisterErr  error
}

func (fake *fakeRegistrar) RegisterOllamaModel(name string, _ bool) error {
	fake.registered = append(fake.registered, name)
	return fake.registerErr
}

func (fake *fakeRegistrar) UnregisterOllamaModel(name string) error {
	fake.unregistered = append(fake.unregistered, name)
	return fake.unregErr
}

func (fake *fakeRegistrar) RegisterDockerModelRunnerModel(alias, model string, _ bool) error {
	fake.dmrRegistered = append(fake.dmrRegistered, alias+"="+model)
	return fake.dmrRegisterErr
}

func (fake *fakeRegistrar) UnregisterDockerModelRunnerModel(alias string) error {
	fake.dmrUnregistered = append(fake.dmrUnregistered, alias)
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

// withDMRAvailable forces the DMR availability probe to a fixed result (no network),
// restoring it after the test.
func withDMRAvailable(test *testing.T, available bool) {
	test.Helper()
	prev := dmrAvailable
	dmrAvailable = func() bool { return available }
	test.Cleanup(func() { dmrAvailable = prev })
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

// --- runtime selection (Ollama vs Docker Model Runner) ----------------------

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
	if len(registrar.dmrRegistered) != 0 {
		test.Fatalf("DMR registration should not run for the default runtime, got %v", registrar.dmrRegistered)
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

// --runtime docker-model-runner is rejected (exit 3) when DMR is unavailable —
// never a silent fallback to Ollama.
func TestModelsPullDMRUnavailableExits3(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	withDMRAvailable(test, false)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "docker-model-runner", "llama3.2:3b")
	if exit != output.ExitMissingDep {
		test.Fatalf("DMR unavailable: exit = %d, want %d", exit, output.ExitMissingDep)
	}
	if fake.PulledName != "" {
		test.Fatalf("no model should be pulled when DMR is rejected, pulled %q", fake.PulledName)
	}
	if len(registrar.registered) != 0 || len(registrar.dmrRegistered) != 0 {
		test.Fatalf("no registration should run when DMR is rejected (no fallback)")
	}
}

// A DMR pull downloads via Ollama then registers via the DMR backend, with the
// default alias derived from the ref's base name.
func TestModelsPullDMRRegistersViaDMR(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	withDMRAvailable(test, true)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "docker-model-runner", "hf.co/acme/qwen:7b")
	if exit != output.ExitOK {
		test.Fatalf("DMR pull exit = %d, want 0", exit)
	}
	if fake.PulledName != "hf.co/acme/qwen:7b" {
		test.Fatalf("Ollama should still download the model, pulled %q", fake.PulledName)
	}
	if len(registrar.registered) != 0 {
		test.Fatalf("Ollama registration should not run for a DMR pull, got %v", registrar.registered)
	}
	// Default alias is the ref's base name (last path segment).
	want := "qwen:7b=hf.co/acme/qwen:7b"
	if len(registrar.dmrRegistered) != 1 || registrar.dmrRegistered[0] != want {
		test.Fatalf("DMR registered = %v, want [%s]", registrar.dmrRegistered, want)
	}
}

// --alias overrides the default DMR gateway alias.
func TestModelsPullDMRCustomAlias(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	withDMRAvailable(test, true)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "docker-model-runner", "--alias", "myqwen", "qwen2.5:7b")
	if exit != output.ExitOK {
		test.Fatalf("DMR pull exit = %d, want 0", exit)
	}
	want := "myqwen=qwen2.5:7b"
	if len(registrar.dmrRegistered) != 1 || registrar.dmrRegistered[0] != want {
		test.Fatalf("DMR registered = %v, want [%s]", registrar.dmrRegistered, want)
	}
}

// --alias with more than one model is rejected (exit 2).
func TestModelsPullAliasMultiExits2(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	withDMRAvailable(test, true)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "docker-model-runner", "--alias", "x", "a:1", "b:2")
	if exit != output.ExitInvalidInput {
		test.Fatalf("--alias with multiple models: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// A model pulled via DMR is de-registered via the DMR backend on rm, and its
// recorded runtime choice is cleared.
func TestModelsRmDeregistersDMR(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar) // sets an isolated HOME for the choice store
	withDMRAvailable(test, true)

	// Pull it via DMR (records the choice under the derived alias).
	exit := output.ExitOK
	pull := newModelsPullCmd(jsonEmitter(), &exit)
	pull.SetOut(io.Discard)
	pull.SetErr(io.Discard)
	runLocalModelsCmd(test, pull, "--runtime", "docker-model-runner", "qwen2.5:7b")
	if exit != output.ExitOK {
		test.Fatalf("DMR pull exit = %d, want 0", exit)
	}

	// Now remove it — the DMR backend must be de-registered by its alias.
	exit = output.ExitOK
	rm := newModelsRmCmd(jsonEmitter(), &exit)
	rm.SetOut(io.Discard)
	rm.SetErr(io.Discard)
	runLocalModelsCmd(test, rm, "qwen2.5:7b")
	if exit != output.ExitOK {
		test.Fatalf("rm exit = %d, want 0", exit)
	}
	if len(registrar.dmrUnregistered) != 1 || registrar.dmrUnregistered[0] != "qwen2.5:7b" {
		test.Fatalf("DMR unregistered = %v, want [qwen2.5:7b]", registrar.dmrUnregistered)
	}
	if len(registrar.unregistered) != 0 {
		test.Fatalf("Ollama unregister should not run for a DMR model, got %v", registrar.unregistered)
	}
	// The choice must be cleared.
	if choices := runtimeChoicesForModel("qwen2.5:7b"); len(choices) != 0 {
		test.Fatalf("runtime choice should be cleared after rm, got %v", choices)
	}
}

// dmrDefaultAlias derives the base name (last path segment) as the DMR alias.
func TestDMRDefaultAlias(test *testing.T) {
	cases := map[string]string{
		"qwen2.5:7b":         "qwen2.5:7b",
		"hf.co/acme/qwen:7b": "qwen:7b",
		"registry/ns/model":  "model",
		"":                   "",
	}
	for ref, want := range cases {
		if got := dmrDefaultAlias(ref); got != want {
			test.Fatalf("dmrDefaultAlias(%q) = %q, want %q", ref, got, want)
		}
	}
}
