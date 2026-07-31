package cli

import (
	"errors"
	"io"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
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

	// vLLM registration recording (mirrors the Ollama fields). vllmRegistered captures
	// the "alias|model|apiBase" of each RegisterVLLMModel call so tests can assert the
	// endpoint threaded through; vllmUnregistered captures the aliases removed.
	vllmRegistered   []string
	vllmUnregistered []string
	vllmRegisterErr  error
	vllmUnregErr     error
}

func (fake *fakeRegistrar) RegisterOllamaModel(name string, _ bool) error {
	fake.registered = append(fake.registered, name)
	return fake.registerErr
}

func (fake *fakeRegistrar) UnregisterOllamaModel(name string) error {
	fake.unregistered = append(fake.unregistered, name)
	return fake.unregErr
}

func (fake *fakeRegistrar) RegisterVLLMModel(alias, model, apiBase string, _ bool) error {
	fake.vllmRegistered = append(fake.vllmRegistered, alias+"|"+model+"|"+apiBase)
	return fake.vllmRegisterErr
}

func (fake *fakeRegistrar) UnregisterVLLMModel(alias string) error {
	fake.vllmUnregistered = append(fake.vllmUnregistered, alias)
	return fake.vllmUnregErr
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

// --- vLLM runtime ------------------------------------------------------------

// fakeVLLMServer is an in-test vllmServer: EnsureServed returns a fixed loopback port +
// endpoint and records the (alias, model) it was asked to serve. It also captures the
// reserved alias→port seed the factory was built with, so a test can assert cross-invocation
// port seeding.
type fakeVLLMServer struct {
	port     int
	endpoint string
	serveErr error
	served   []string       // "alias|model"
	reserved map[string]int // the seed the factory received
}

func (fake *fakeVLLMServer) EnsureServed(alias, model string) (int, string, error) {
	fake.served = append(fake.served, alias+"|"+model)
	if fake.serveErr != nil {
		return 0, "", fake.serveErr
	}
	return fake.port, fake.endpoint, nil
}

// withFakeVLLM swaps the host-side vLLM seams for the duration of a test: detection,
// the (best-effort) Pull, the server-manager factory (which records its reserved seed),
// and the by-port stop seam (which records the ports it was asked to stop).
func withFakeVLLM(test *testing.T, installed bool, pullErr error, server *fakeVLLMServer) *[]int {
	test.Helper()
	prevDetect, prevPull, prevFactory, prevStop := vllmDetectFn, vllmPullFn, vllmManagerFactory, vllmStopByPortFn
	vllmDetectFn = func() (bool, string) { return installed, "mlx" }
	vllmPullFn = func(string) error { return pullErr }
	vllmManagerFactory = func(reserved map[string]int) vllmServer {
		server.reserved = reserved
		return server
	}
	stoppedPorts := &[]int{}
	vllmStopByPortFn = func(port int) error {
		*stoppedPorts = append(*stoppedPorts, port)
		return nil
	}
	test.Cleanup(func() {
		vllmDetectFn, vllmPullFn, vllmManagerFactory, vllmStopByPortFn = prevDetect, prevPull, prevFactory, prevStop
	})
	return stoppedPorts
}

// withFakeVLLMInstaller swaps the one-shot vLLM installer seam, recording the specs it
// was handed and returning installErr. The recorded slice lets a test assert the per-OS
// default (empty --spec) vs an explicit --spec override reaches the installer.
func withFakeVLLMInstaller(test *testing.T, installErr error) *[][]string {
	test.Helper()
	prev := vllmInstallFn
	calls := &[][]string{}
	vllmInstallFn = func(specs ...string) error {
		*calls = append(*calls, append([]string(nil), specs...))
		return installErr
	}
	test.Cleanup(func() { vllmInstallFn = prev })
	return calls
}

// `ai models install-vllm` with no --spec installs the per-OS default and reports success.
func TestModelsInstallVLLMDefaultSpec(test *testing.T) {
	calls := withFakeVLLMInstaller(test, nil)
	withFakeVLLM(test, true, nil, &fakeVLLMServer{}) // Detect after install → installed
	exit := output.ExitOK
	cmd := newModelsInstallVLLMCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitOK {
		test.Fatalf("install-vllm: exit = %d, want %d", exit, output.ExitOK)
	}
	if len(*calls) != 1 {
		test.Fatalf("installer calls = %d, want 1", len(*calls))
	}
	if want := vllm.InstallSpecs(goruntime.GOOS); !slices.Equal((*calls)[0], want) {
		test.Fatalf("default specs = %v, want per-OS default %v", (*calls)[0], want)
	}
}

// An explicit --spec (repeatable) overrides the per-OS default entirely.
func TestModelsInstallVLLMCustomSpecOverride(test *testing.T) {
	calls := withFakeVLLMInstaller(test, nil)
	withFakeVLLM(test, true, nil, &fakeVLLMServer{})
	exit := output.ExitOK
	cmd := newModelsInstallVLLMCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--spec", "vllm==0.6.0", "--spec", "--upgrade")
	if exit != output.ExitOK {
		test.Fatalf("install-vllm custom spec: exit = %d, want %d", exit, output.ExitOK)
	}
	want := []string{"vllm==0.6.0", "--upgrade"}
	if len(*calls) != 1 || !slices.Equal((*calls)[0], want) {
		test.Fatalf("custom specs = %v, want %v", *calls, want)
	}
}

// A failed install surfaces exit 4 (runtime failure).
func TestModelsInstallVLLMFailureExits4(test *testing.T) {
	withFakeVLLMInstaller(test, errors.New("no space left on device"))
	exit := output.ExitOK
	cmd := newModelsInstallVLLMCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("failed install: exit = %d, want %d", exit, output.ExitRuntimeFailure)
	}
}

// --runtime vllm with vLLM absent exits 3 with install guidance — NEVER a silent
// downgrade to Ollama.
func TestModelsPullVLLMNotInstalledExits3(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	withFakeVLLM(test, false, nil, &fakeVLLMServer{})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "vllm", "mlx-community/Qwen2.5-7B-Instruct-4bit")
	if exit != output.ExitMissingDep {
		test.Fatalf("vLLM absent: exit = %d, want %d", exit, output.ExitMissingDep)
	}
	if fake.PulledName != "" {
		test.Fatalf("no fallback to Ollama: pulled %q", fake.PulledName)
	}
	if len(registrar.registered) != 0 || len(registrar.vllmRegistered) != 0 {
		test.Fatalf("nothing should be registered when vLLM is absent: ollama=%v vllm=%v",
			registrar.registered, registrar.vllmRegistered)
	}
}

// --runtime vllm registers the model with the CONTAINER api_base (host.docker.internal —
// what LiteLLM in a container must dial), NOT the loopback, and records the LOOPBACK
// endpoint (host-side truth) as the runtime choice.
func TestModelsPullVLLMRegistersWithEndpoint(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	server := &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"}
	withFakeVLLM(test, true, nil, server)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "vllm", "--alias", "my-qwen", "mlx-community/Qwen2.5-7B-Instruct-4bit")
	if exit != output.ExitOK {
		test.Fatalf("vLLM pull exit = %d, want 0", exit)
	}
	// The gateway api_base is the CONTAINER endpoint (host.docker.internal), not loopback.
	wantReg := "my-qwen|mlx-community/Qwen2.5-7B-Instruct-4bit|http://host.docker.internal:8101/v1"
	if len(registrar.vllmRegistered) != 1 || registrar.vllmRegistered[0] != wantReg {
		test.Fatalf("vLLM registered = %v, want [%s]", registrar.vllmRegistered, wantReg)
	}
	if len(server.served) != 1 || server.served[0] != "my-qwen|mlx-community/Qwen2.5-7B-Instruct-4bit" {
		test.Fatalf("EnsureServed calls = %v", server.served)
	}
	// The recorded runtime choice carries the LOOPBACK endpoint (host-side truth for probes
	// + stop-by-port), not the container form.
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || choice.Runtime != config.RuntimeVLLM || choice.Endpoint != "http://127.0.0.1:8101/v1" {
		test.Fatalf("recorded choice = %+v (ok=%v), want vllm @ loopback endpoint", choice, ok)
	}
}

// A vLLM pull whose gateway registration FAILS still RECORDS the runtime choice (on start,
// fix #5) so a later `rm` can find and stop the running server; the outcome carries the
// register error but the pull as a whole is not a hard failure (the server IS up).
func TestModelsPullVLLMRecordsEvenWhenRegisterFails(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{vllmRegisterErr: errors.New("gateway down")}
	withFakeRegistrar(test, registrar)
	server := &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"}
	withFakeVLLM(test, true, nil, server)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "vllm", "--alias", "my-qwen", "mlx-community/Qwen3-8B")
	// Register failed but the server started → the choice is recorded regardless.
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || choice.Endpoint != "http://127.0.0.1:8101/v1" {
		test.Fatalf("choice must be recorded on start even when register fails: %+v (ok=%v)", choice, ok)
	}
}

// The Manager factory is seeded with the recorded alias→port map so a KNOWN alias reuses
// its port and a new one never steals a recorded port (cross-invocation port truth).
func TestModelsPullVLLMSeedsReservedPorts(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	// Pre-seed the store with an existing vLLM model on port 8101.
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias:    "existing",
		Model:    "mlx-community/Existing",
		Runtime:  config.RuntimeVLLM,
		Endpoint: "http://127.0.0.1:8101/v1",
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}
	server := &fakeVLLMServer{port: 8102, endpoint: "http://127.0.0.1:8102/v1"}
	withFakeVLLM(test, true, nil, server)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "vllm", "--alias", "fresh", "mlx-community/Fresh")
	if server.reserved["existing"] != 8101 {
		test.Fatalf("factory reserved seed = %v, want existing→8101 parsed from the recorded endpoint", server.reserved)
	}
}

// --alias with more than one model is a usage error (exit 2).
func TestModelsPullVLLMAliasMultiModelExits2(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	withFakeVLLM(test, true, nil, &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "vllm", "--alias", "x", "a", "b")
	if exit != output.ExitInvalidInput {
		test.Fatalf("--alias with 2 models: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// Without --alias the gateway alias defaults to the model id's base name.
func TestModelsPullVLLMDefaultAlias(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	withFakeVLLM(test, true, nil, &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "vllm", "mlx-community/Foo-Bar")
	if exit != output.ExitOK {
		test.Fatalf("vLLM pull exit = %d, want 0", exit)
	}
	if len(registrar.vllmRegistered) != 1 || !strings.HasPrefix(registrar.vllmRegistered[0], "Foo-Bar|") {
		test.Fatalf("default alias not Foo-Bar: %v", registrar.vllmRegistered)
	}
}

// A vLLM pull whose serve step is not wired (ErrNotWired) surfaces an actionable
// runtime failure rather than crashing.
func TestModelsPullVLLMServeNotWired(test *testing.T) {
	withFakeOllama(test, &ollama.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	withFakeVLLM(test, true, nil, &fakeVLLMServer{serveErr: vllm.ErrNotWired})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--runtime", "vllm", "mlx-community/Foo")
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("not-wired serve: exit = %d, want %d", exit, output.ExitRuntimeFailure)
	}
}

// rm of a vLLM-recorded model de-registers via UnregisterVLLMModel, stops the detached
// server by its RECORDED PORT (a fresh Manager holds no handle), clears the recorded
// choice, and does NOT touch the Ollama store.
func TestModelsRmVLLMDeregistersAndStops(test *testing.T) {
	fake := &ollama.Fake{}
	withFakeOllama(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	server := &fakeVLLMServer{}
	stoppedPorts := withFakeVLLM(test, true, nil, server)
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias:    "my-qwen",
		Model:    "mlx-community/Qwen2.5-7B-Instruct-4bit",
		Runtime:  config.RuntimeVLLM,
		Endpoint: "http://127.0.0.1:8101/v1",
	}); err != nil {
		test.Fatalf("seed runtime choice: %v", err)
	}
	exit := output.ExitOK
	cmd := newModelsRmCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "my-qwen")
	if exit != output.ExitOK {
		test.Fatalf("vLLM rm exit = %d, want 0", exit)
	}
	if len(registrar.vllmUnregistered) != 1 || registrar.vllmUnregistered[0] != "my-qwen" {
		test.Fatalf("vLLM unregistered = %v, want [my-qwen]", registrar.vllmUnregistered)
	}
	if len(*stoppedPorts) != 1 || (*stoppedPorts)[0] != 8101 {
		test.Fatalf("StopByPort ports = %v, want [8101] (parsed from the recorded endpoint)", *stoppedPorts)
	}
	if len(registrar.unregistered) != 0 {
		test.Fatalf("Ollama unregister must not be called for a vLLM model: %v", registrar.unregistered)
	}
	if fake.RemovedName != "" {
		test.Fatalf("Ollama store must not be touched for a vLLM model, removed %q", fake.RemovedName)
	}
	if _, ok := config.ModelRuntimeFor("my-qwen"); ok {
		test.Fatal("runtime choice should be cleared after rm")
	}
}
