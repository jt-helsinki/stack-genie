package cli

import (
	"errors"
	"io"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
)

// withFakeHF swaps the package-level hfClient constructor for one returning the given
// fake (no `hf` binary, no network), restoring it after the test.
func withFakeHF(test *testing.T, fake *hf.Fake) {
	test.Helper()
	prev := hfClient
	hfClient = func() hf.Client { return fake }
	test.Cleanup(func() { hfClient = prev })
}

// fakeRegistrar records the vLLM (un)register calls and can be configured to fail (to
// prove pull/rm tolerate a gateway error). vllmRegistered captures "alias|model|apiBase".
type fakeRegistrar struct {
	vllmRegistered   []string
	vllmUnregistered []string
	vllmRegisterErr  error
	vllmUnregErr     error
}

func (fake *fakeRegistrar) RegisterVLLMModel(alias, model, apiBase string, _ bool) error {
	fake.vllmRegistered = append(fake.vllmRegistered, alias+"|"+model+"|"+apiBase)
	return fake.vllmRegisterErr
}

func (fake *fakeRegistrar) UnregisterVLLMModel(alias string) error {
	fake.vllmUnregistered = append(fake.vllmUnregistered, alias)
	return fake.vllmUnregErr
}

// withFakeRegistrar swaps modelRegistrarFactory for one returning the given fake (no
// network) and redirects HOME to a temp dir so the best-effort runtime-choice store
// writes land in an isolated location.
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

func TestInstalledEntriesListsCachedRepos(test *testing.T) {
	entries := installedEntries([]hf.CachedModel{
		{Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit", Size: "4.3 GB"},
		{Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit"},
	})
	if len(entries) != 2 {
		test.Fatalf("got %d entries, want 2", len(entries))
	}
	if !entries[0].Installed || entries[0].Name != "mlx-community/Qwen2.5-7B-Instruct-4bit" || entries[0].Size != "4.3 GB" {
		test.Fatalf("first entry = %+v", entries[0])
	}
}

func TestModelsListError(test *testing.T) {
	withFakeHF(test, &hf.Fake{CacheErr: errors.New("hf not installed")})
	exit := output.ExitOK
	cmd := newModelsListCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("list error exit = %d, want %d", exit, output.ExitRuntimeFailure)
	}
}

func TestModelsListHumanTable(test *testing.T) {
	result := modelsListResult{Models: installedEntries(
		[]hf.CachedModel{{Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit", Size: "4.3 GB"}},
	)}
	human := result.Human()
	for _, want := range []string{"NAME", "SIZE", "mlx-community/Qwen2.5-7B-Instruct-4bit", "4.3 GB"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

func TestModelsPopularSuccess(test *testing.T) {
	exit := output.ExitOK
	cmd := newModelsPopularCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitOK {
		test.Fatalf("popular exit = %d, want 0", exit)
	}
}

func TestModelsPopularHuman(test *testing.T) {
	result := modelsPopularResult{Models: curatedEntries(hf.CuratedModels(goruntime.GOOS))}
	human := result.Human()
	for _, want := range []string{"REPO", "SIZE", "DESCRIPTION"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

func TestParseModelRefs(test *testing.T) {
	got := parseModelRefs("a/b c/d, a/b\te/f")
	want := []string{"a/b", "c/d", "e/f"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		test.Fatalf("parseModelRefs = %v, want %v", got, want)
	}
	if len(parseModelRefs("   ,  ,")) != 0 {
		test.Fatalf("parseModelRefs of separators-only should be empty")
	}
}

func TestModelsPullResultHuman(test *testing.T) {
	result := modelsPullResult{Pulled: []modelPullOutcome{
		{Model: "mlx-community/Foo", OK: true},
		{Model: "mlx-community/Bad", OK: false, Error: "boom"},
	}}
	human := result.Human()
	for _, want := range []string{"mlx-community/Foo", "mlx-community/Bad", "boom"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

func TestModelsPullResultHumanRegisterWarning(test *testing.T) {
	result := modelsPullResult{Pulled: []modelPullOutcome{
		{Model: "mlx-community/Foo", OK: true, RegisterError: "gateway down"},
	}}
	human := result.Human()
	for _, want := range []string{"mlx-community/Foo", "gateway down", "registration"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

func TestModelsPullMissingNameNonInteractiveExits2(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitInvalidInput {
		test.Fatalf("pull with no name under --json: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// --- vLLM (the sole local runtime) -------------------------------------------

// fakeVLLMServer is an in-test vllmServer: EnsureServed returns a fixed loopback port +
// endpoint and records the (alias, model) it was asked to serve, plus the reserved seed.
type fakeVLLMServer struct {
	port     int
	endpoint string
	serveErr error
	served   []string
	reserved map[string]int
}

func (fake *fakeVLLMServer) EnsureServed(alias, model string) (int, string, error) {
	fake.served = append(fake.served, alias+"|"+model)
	if fake.serveErr != nil {
		return 0, "", fake.serveErr
	}
	return fake.port, fake.endpoint, nil
}

// withFakeVLLM swaps the host-side vLLM seams: detection, the (best-effort) Pull, the
// server-manager factory (recording its reserved seed), and the by-port stop seam.
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

// withFakeVLLMInstaller swaps the one-shot vLLM installer seam, recording specs.
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

func TestModelsInstallVLLMDefaultSpec(test *testing.T) {
	calls := withFakeVLLMInstaller(test, nil)
	withFakeVLLM(test, true, nil, &fakeVLLMServer{})
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

// A pull with vLLM absent exits 3 with install guidance — never a silent no-op.
func TestModelsPullVLLMNotInstalledExits3(test *testing.T) {
	fake := &hf.Fake{}
	withFakeHF(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	withFakeVLLM(test, false, nil, &fakeVLLMServer{})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "mlx-community/Qwen2.5-7B-Instruct-4bit")
	if exit != output.ExitMissingDep {
		test.Fatalf("vLLM absent: exit = %d, want %d", exit, output.ExitMissingDep)
	}
	if fake.DownloadedRepo != "" {
		test.Fatalf("no download when vLLM is absent, got %q", fake.DownloadedRepo)
	}
	if len(registrar.vllmRegistered) != 0 {
		test.Fatalf("nothing should be registered when vLLM is absent: %v", registrar.vllmRegistered)
	}
}

// A pull downloads via hf, serves via vLLM, and registers with the CONTAINER api_base
// (host.docker.internal — what LiteLLM in a container dials), recording the LOOPBACK
// endpoint as the runtime choice.
func TestModelsPullVLLMRegistersWithEndpoint(test *testing.T) {
	fake := &hf.Fake{}
	withFakeHF(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	server := &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"}
	withFakeVLLM(test, true, nil, server)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--alias", "my-qwen", "mlx-community/Qwen2.5-7B-Instruct-4bit")
	if exit != output.ExitOK {
		test.Fatalf("vLLM pull exit = %d, want 0", exit)
	}
	if fake.DownloadedRepo != "mlx-community/Qwen2.5-7B-Instruct-4bit" {
		test.Fatalf("downloaded %q, want the repo", fake.DownloadedRepo)
	}
	wantReg := "my-qwen|mlx-community/Qwen2.5-7B-Instruct-4bit|http://host.docker.internal:8101/v1"
	if len(registrar.vllmRegistered) != 1 || registrar.vllmRegistered[0] != wantReg {
		test.Fatalf("vLLM registered = %v, want [%s]", registrar.vllmRegistered, wantReg)
	}
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || choice.Runtime != config.RuntimeVLLM || choice.Endpoint != "http://127.0.0.1:8101/v1" {
		test.Fatalf("recorded choice = %+v (ok=%v), want vllm @ loopback endpoint", choice, ok)
	}
}

func TestModelsPullVLLMRecordsEvenWhenRegisterFails(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	registrar := &fakeRegistrar{vllmRegisterErr: errors.New("gateway down")}
	withFakeRegistrar(test, registrar)
	server := &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"}
	withFakeVLLM(test, true, nil, server)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--alias", "my-qwen", "mlx-community/Qwen3-8B")
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || choice.Endpoint != "http://127.0.0.1:8101/v1" {
		test.Fatalf("choice must be recorded on start even when register fails: %+v (ok=%v)", choice, ok)
	}
}

func TestModelsPullVLLMSeedsReservedPorts(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "existing", Model: "mlx-community/Existing",
		Runtime: config.RuntimeVLLM, Endpoint: "http://127.0.0.1:8101/v1",
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}
	server := &fakeVLLMServer{port: 8102, endpoint: "http://127.0.0.1:8102/v1"}
	withFakeVLLM(test, true, nil, server)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--alias", "fresh", "mlx-community/Fresh")
	if server.reserved["existing"] != 8101 {
		test.Fatalf("factory reserved seed = %v, want existing→8101 parsed from the recorded endpoint", server.reserved)
	}
}

func TestModelsPullVLLMAliasMultiModelExits2(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	withFakeVLLM(test, true, nil, &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--alias", "x", "a/one", "b/two")
	if exit != output.ExitInvalidInput {
		test.Fatalf("--alias with 2 models: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestModelsPullVLLMDefaultAlias(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	withFakeVLLM(test, true, nil, &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "mlx-community/Foo-Bar")
	if exit != output.ExitOK {
		test.Fatalf("vLLM pull exit = %d, want 0", exit)
	}
	if len(registrar.vllmRegistered) != 1 || !strings.HasPrefix(registrar.vllmRegistered[0], "Foo-Bar|") {
		test.Fatalf("default alias not Foo-Bar: %v", registrar.vllmRegistered)
	}
}

func TestModelsPullVLLMServeNotWired(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	withFakeVLLM(test, true, nil, &fakeVLLMServer{serveErr: vllm.ErrNotWired})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "mlx-community/Foo")
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("not-wired serve: exit = %d, want %d", exit, output.ExitRuntimeFailure)
	}
}

// rm of a vLLM-recorded model de-registers, stops the server by its RECORDED PORT,
// deletes the weights from the HF cache, and clears the recorded choice.
func TestModelsRmVLLMDeregistersAndStops(test *testing.T) {
	fake := &hf.Fake{}
	withFakeHF(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	server := &fakeVLLMServer{}
	stoppedPorts := withFakeVLLM(test, true, nil, server)
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "my-qwen", Model: "mlx-community/Qwen2.5-7B-Instruct-4bit",
		Runtime: config.RuntimeVLLM, Endpoint: "http://127.0.0.1:8101/v1",
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
		test.Fatalf("StopByPort ports = %v, want [8101]", *stoppedPorts)
	}
	if fake.RemovedRepo != "mlx-community/Qwen2.5-7B-Instruct-4bit" {
		test.Fatalf("hf cache rm repo = %q, want the recorded model", fake.RemovedRepo)
	}
	if _, ok := config.ModelRuntimeFor("my-qwen"); ok {
		test.Fatal("runtime choice should be cleared after rm")
	}
}

// show returns curated metadata for a curated repo even with no recorded choice.
func TestModelsShowCurated(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	repo := hf.CuratedModels(goruntime.GOOS)[0].Repo
	exit := output.ExitOK
	cmd := newModelsShowCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, repo)
	if exit != output.ExitOK {
		test.Fatalf("show exit = %d, want 0", exit)
	}
}
