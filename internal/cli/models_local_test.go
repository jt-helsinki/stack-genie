package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
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

// withFakeHFDetect forces the `hf`-installed probe to the given value, restoring it
// after the test (so the login/logout dep gate is exercised without a real `hf`).
func withFakeHFDetect(test *testing.T, installed bool) {
	test.Helper()
	prev := hfDetectFn
	hfDetectFn = func() bool { return installed }
	test.Cleanup(func() { hfDetectFn = prev })
}

func TestModelsLoginSuccess(test *testing.T) {
	withFakeHFDetect(test, true)
	fake := &hf.Fake{WhoamiUser: "alice"}
	withFakeHF(test, fake)
	exit := output.ExitOK
	cmd := newModelsLoginCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--token", "hf_tok_x")
	if exit != output.ExitOK {
		test.Fatalf("login exit = %d, want 0", exit)
	}
	if fake.LoginToken != "hf_tok_x" {
		test.Fatalf("login token = %q, want hf_tok_x", fake.LoginToken)
	}
}

func TestModelsLoginNotInstalled(test *testing.T) {
	withFakeHFDetect(test, false)
	withFakeHF(test, &hf.Fake{})
	exit := output.ExitOK
	cmd := newModelsLoginCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--token", "x")
	if exit != output.ExitMissingDep {
		test.Fatalf("login (no hf) exit = %d, want %d", exit, output.ExitMissingDep)
	}
}

func TestModelsLoginEmptyTokenNonInteractive(test *testing.T) {
	withFakeHFDetect(test, true)
	withFakeHF(test, &hf.Fake{})
	exit := output.ExitOK
	cmd := newModelsLoginCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd) // no --token, --json/no-TTY → can't prompt
	if exit != output.ExitInvalidInput {
		test.Fatalf("login (empty token) exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestModelsLoginError(test *testing.T) {
	withFakeHFDetect(test, true)
	withFakeHF(test, &hf.Fake{LoginErr: errors.New("invalid token")})
	exit := output.ExitOK
	cmd := newModelsLoginCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--token", "x")
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("login error exit = %d, want %d", exit, output.ExitRuntimeFailure)
	}
}

func TestModelsLogoutSuccess(test *testing.T) {
	withFakeHFDetect(test, true)
	fake := &hf.Fake{}
	withFakeHF(test, fake)
	exit := output.ExitOK
	cmd := newModelsLogoutCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitOK {
		test.Fatalf("logout exit = %d, want 0", exit)
	}
	if fake.LogoutCalls != 1 {
		test.Fatalf("LogoutCalls = %d, want 1", fake.LogoutCalls)
	}
}

func TestModelsLogoutNotInstalled(test *testing.T) {
	withFakeHFDetect(test, false)
	withFakeHF(test, &hf.Fake{})
	exit := output.ExitOK
	cmd := newModelsLogoutCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitMissingDep {
		test.Fatalf("logout (no hf) exit = %d, want %d", exit, output.ExitMissingDep)
	}
}

func TestModelsLogoutError(test *testing.T) {
	withFakeHFDetect(test, true)
	withFakeHF(test, &hf.Fake{LogoutErr: errors.New("logout failed")})
	exit := output.ExitOK
	cmd := newModelsLogoutCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd)
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("logout error exit = %d, want %d", exit, output.ExitRuntimeFailure)
	}
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

// withCuratedProvider swaps the curated-list seam so `ai models popular` tests run
// with a fake list (no network), restoring it afterward.
func withCuratedProvider(test *testing.T, fn func(string) []hf.CuratedModel) {
	test.Helper()
	prev := curatedModelsProvider
	curatedModelsProvider = fn
	test.Cleanup(func() { curatedModelsProvider = prev })
}

// runPopular runs `ai models popular` with the given args over a JSON emitter and
// decodes the envelope's data payload.
func runPopular(test *testing.T, args ...string) (int, modelsPopularResult) {
	test.Helper()
	var out bytes.Buffer
	emitter := &output.Emitter{Out: &out, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newModelsPopularCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, args...)
	var envelope struct {
		Data modelsPopularResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		test.Fatalf("decode envelope: %v\n%s", err, out.String())
	}
	return exit, envelope.Data
}

// `ai models popular` reads straight from the injected curated-list seam — a static
// set, no live fetch/cache/refresh involved.
func TestModelsPopularReadsCuratedSeam(test *testing.T) {
	withCuratedProvider(test, func(_ string) []hf.CuratedModel {
		return []hf.CuratedModel{{Name: "Qwen3-8B-4bit", Repo: "mlx-community/Qwen3-8B-4bit", Description: "hot"}}
	})
	exit, data := runPopular(test)
	if exit != output.ExitOK {
		test.Fatalf("exit = %d, want 0", exit)
	}
	if len(data.Models) != 1 || data.Models[0].Repo != "mlx-community/Qwen3-8B-4bit" {
		test.Fatalf("models = %+v, want the seeded repo", data.Models)
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
	opts     []vllm.ServeOptions
	reserved map[string]int
}

func (fake *fakeVLLMServer) EnsureServedWithOptions(alias, model string, opts vllm.ServeOptions) (int, string, error) {
	fake.served = append(fake.served, alias+"|"+model+"|"+opts.ToolCallParser)
	fake.opts = append(fake.opts, opts)
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
	vllmManagerFactory = func(reserved map[string]int, _ func(string)) vllmServer {
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
	if choice.Status != "registered" {
		test.Fatalf("choice.Status = %q, want %q on a successful register", choice.Status, "registered")
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
	if choice.Status != "registration-failed" {
		test.Fatalf("choice.Status = %q, want %q (must reflect the actual register failure, not an assumed success)",
			choice.Status, "registration-failed")
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

// TestVLLMGPUMemoryUtilizationBudgetWarning proves the sum-across-all-recorded-models
// check: vLLM reserves each model's --gpu-memory-utilization fraction of TOTAL device
// memory INDEPENDENTLY at its own startup, so two models each pinned 0.5 (a real
// misconfiguration reported against this platform) must warn, while a single model or
// a safe combined total must not.
// TestVLLMLastLogLines proves the failure-diagnostics tail helper: bounds to the
// last N lines, and reads "" for a missing/empty file rather than erroring — the
// fix for a truncated single-line spinner status hiding the actual crash reason
// (e.g. an MLX/CUDA engine-init RuntimeError) on a failed vLLM start.
func TestVLLMLastLogLines(test *testing.T) {
	dir := test.TempDir()
	path := dir + "/model.log"
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\nline4\n"), 0o644); err != nil {
		test.Fatalf("write log: %v", err)
	}
	if got := vllmLastLogLines(path, 2); got != "line3\nline4" {
		test.Fatalf("vllmLastLogLines(n=2) = %q, want the last 2 lines", got)
	}
	if got := vllmLastLogLines(path, 10); got != "line1\nline2\nline3\nline4" {
		test.Fatalf("vllmLastLogLines(n=10) = %q, want the whole file (fewer lines than n)", got)
	}
	if got := vllmLastLogLines(dir+"/missing.log", 5); got != "" {
		test.Fatalf("missing file: got %q, want \"\"", got)
	}
}

func TestVLLMGPUMemoryUtilizationBudgetWarning(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	// Only one model, even a high value: nothing to sum against, no warning.
	if warning := vllmGPUMemoryUtilizationBudgetWarning("solo", 0.9); warning != "" {
		test.Fatalf("single model: want no warning, got %q", warning)
	}

	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "existing", Model: "mlx-community/Existing", Runtime: config.RuntimeVLLM,
		Endpoint: "http://127.0.0.1:8101/v1", GPUMemoryUtilization: 0.5,
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}

	// 0.5 (existing) + 0.5 (new) = 1.0 > the 0.9 safe budget: must warn.
	warning := vllmGPUMemoryUtilizationBudgetWarning("new", 0.5)
	if warning == "" {
		test.Fatal("0.5 + 0.5 = 1.0 over the 0.9 safe budget: want a warning, got none")
	}
	if !strings.Contains(warning, "ai models configure") {
		test.Errorf("warning should point at the remediation command, got %q", warning)
	}

	// 0.5 (existing) + 0.3 (new) = 0.8, under budget: no warning.
	if warning := vllmGPUMemoryUtilizationBudgetWarning("new", 0.3); warning != "" {
		test.Fatalf("0.5 + 0.3 = 0.8 under budget: want no warning, got %q", warning)
	}

	// Re-checking the ALREADY-recorded "existing" alias against itself must exclude
	// its own current value from the "others" sum, so raising it to 0.6 alone (no
	// other models) is NOT over budget.
	if warning := vllmGPUMemoryUtilizationBudgetWarning("existing", 0.6); warning != "" {
		test.Fatalf("reconfiguring the only recorded model must not sum against itself, got %q", warning)
	}
}

// TestModelsPullVLLMWarnsOnOverBudgetGPUMemoryUtilization proves the warning surfaces
// on the actual `ai models pull` JSON envelope, not just the unit-level helper.
func TestModelsPullVLLMWarnsOnOverBudgetGPUMemoryUtilization(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "existing", Model: "mlx-community/Existing", Runtime: config.RuntimeVLLM,
		Endpoint: "http://127.0.0.1:8101/v1", GPUMemoryUtilization: 0.5,
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}
	withFakeVLLM(test, true, nil, &fakeVLLMServer{port: 8102, endpoint: "http://127.0.0.1:8102/v1"})

	var buffer bytes.Buffer
	emitter := &output.Emitter{Out: &buffer, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newModelsPullCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--alias", "new", "--gpu-memory-utilization", "0.5", "mlx-community/New")

	if exit != output.ExitOK {
		test.Fatalf("pull exit = %d, want 0 (a warning must not block the pull)", exit)
	}
	var envelope struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(buffer.Bytes(), &envelope); err != nil {
		test.Fatalf("decode envelope: %v (body: %s)", err, buffer.String())
	}
	if len(envelope.Warnings) != 1 || !strings.Contains(envelope.Warnings[0], "gpu-memory-utilization") {
		test.Fatalf("envelope.warnings = %v, want one gpu-memory-utilization budget warning", envelope.Warnings)
	}
}

// TestModelsPullVLLMResourceOptionsFlagsThreadThrough proves --gpu-memory-utilization
// and --max-model-len reach EnsureServedWithOptions AND are persisted in the recorded
// runtime choice, so a later restart relaunches the model with the same caps instead of
// falling back to vLLM's own default (0.9 utilization, sized off total device memory —
// the fix for a small model resident at tens of GB).
func TestModelsPullVLLMResourceOptionsFlagsThreadThrough(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	server := &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"}
	withFakeVLLM(test, true, nil, server)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--alias", "my-qwen", "--gpu-memory-utilization", "0.5",
		"--max-model-len", "8192", "mlx-community/Qwen3-8B")
	if exit != output.ExitOK {
		test.Fatalf("vLLM pull exit = %d, want 0", exit)
	}
	if len(server.opts) != 1 || server.opts[0].GPUMemoryUtilization != 0.5 || server.opts[0].MaxModelLen != 8192 {
		test.Fatalf("EnsureServedWithOptions opts = %+v, want {GPUMemoryUtilization:0.5 MaxModelLen:8192}", server.opts)
	}
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || choice.GPUMemoryUtilization != 0.5 || choice.MaxModelLen != 8192 {
		test.Fatalf("recorded choice = %+v (ok=%v), want the resource caps persisted for a later restart", choice, ok)
	}
}

// TestModelsPullVLLMResourceOptionsRejectsInvalid proves an out-of-range
// --gpu-memory-utilization is rejected (exit 2) rather than silently passed to vLLM.
func TestModelsPullVLLMResourceOptionsRejectsInvalid(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	withFakeVLLM(test, true, nil, &fakeVLLMServer{})
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--gpu-memory-utilization", "1.5", "mlx-community/Qwen3-8B")
	if exit != output.ExitInvalidInput {
		test.Fatalf("exit = %d, want ExitInvalidInput for an out-of-range --gpu-memory-utilization", exit)
	}
}

// TestModelsConfigureAppliesCapsWithoutDownloading proves `ai models configure`
// changes the recorded resource caps and restarts the server WITHOUT touching the `hf`
// download path (the fake's Download call count must stay 0) — the whole point being
// "set these without re-pulling the model".
func TestModelsConfigureAppliesCapsWithoutDownloading(test *testing.T) {
	fakeHF := &hf.Fake{}
	withFakeHF(test, fakeHF)
	withFakeRegistrar(test, &fakeRegistrar{})
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "my-qwen", Model: "mlx-community/Qwen3-8B",
		Runtime: config.RuntimeVLLM, Endpoint: "http://127.0.0.1:8101/v1", Status: "registered",
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}
	server := &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"}
	withFakeVLLM(test, true, nil, server)
	var stoppedPorts []int
	restore := vllmStopByPortFn
	vllmStopByPortFn = func(port int) error { stoppedPorts = append(stoppedPorts, port); return nil }
	defer func() { vllmStopByPortFn = restore }()

	exit := output.ExitOK
	cmd := newModelsConfigureCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--gpu-memory-utilization", "0.5", "--max-model-len", "8192", "my-qwen")
	if exit != output.ExitOK {
		test.Fatalf("configure exit = %d, want 0", exit)
	}
	if len(fakeHF.DownloadedAll) != 0 {
		test.Fatalf("Download called %d times, want 0 (configure must not re-pull weights)", len(fakeHF.DownloadedAll))
	}
	if len(stoppedPorts) != 1 || stoppedPorts[0] != 8101 {
		test.Fatalf("stopped ports = %v, want [8101] (a running server must be stopped, not adopted, for new caps to apply)", stoppedPorts)
	}
	if len(server.opts) != 1 || server.opts[0].GPUMemoryUtilization != 0.5 || server.opts[0].MaxModelLen != 8192 {
		test.Fatalf("EnsureServedWithOptions opts = %+v, want {GPUMemoryUtilization:0.5 MaxModelLen:8192}", server.opts)
	}
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || choice.GPUMemoryUtilization != 0.5 || choice.MaxModelLen != 8192 {
		test.Fatalf("recorded choice = %+v (ok=%v), want the new caps persisted", choice, ok)
	}
}

// TestModelsConfigureUnknownModelExits2 proves an unrecorded model is rejected rather
// than silently starting a fresh server for it (that is `ai models pull`'s job).
func TestModelsConfigureUnknownModelExits2(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	withFakeVLLM(test, true, nil, &fakeVLLMServer{})
	exit := output.ExitOK
	cmd := newModelsConfigureCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--gpu-memory-utilization", "0.5", "never-pulled")
	if exit != output.ExitInvalidInput {
		test.Fatalf("exit = %d, want ExitInvalidInput for a model with no recorded vLLM runtime choice", exit)
	}
}

// TestModelsDisableStopsAndRecordsFlag proves `ai models disable` stops the model's
// server (best-effort) and persists Disabled=true, preserving its other recorded
// fields (the resource caps) — this is the mechanism ensureVLLMServers now checks to
// skip a model in its auto-start pass, so two local models' gpu-memory-utilization
// no longer have to sum within budget if only one is meant to run at a time.
func TestModelsDisableStopsAndRecordsFlag(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "my-qwen", Model: "mlx-community/Qwen3-8B", Runtime: config.RuntimeVLLM,
		Endpoint: "http://127.0.0.1:8101/v1", Status: "registered", GPUMemoryUtilization: 0.5,
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}
	withFakeVLLM(test, true, nil, &fakeVLLMServer{})
	var stoppedPorts []int
	restore := vllmStopByPortFn
	vllmStopByPortFn = func(port int) error { stoppedPorts = append(stoppedPorts, port); return nil }
	defer func() { vllmStopByPortFn = restore }()

	exit := output.ExitOK
	cmd := newModelsDisableCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "my-qwen")
	if exit != output.ExitOK {
		test.Fatalf("disable exit = %d, want 0", exit)
	}
	if len(stoppedPorts) != 1 || stoppedPorts[0] != 8101 {
		test.Fatalf("stopped ports = %v, want [8101]", stoppedPorts)
	}
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || !choice.Disabled {
		test.Fatalf("recorded choice = %+v (ok=%v), want Disabled=true", choice, ok)
	}
	if choice.GPUMemoryUtilization != 0.5 {
		test.Errorf("disable must preserve other fields, got GPUMemoryUtilization=%v", choice.GPUMemoryUtilization)
	}
}

// TestModelsEnableStartsAndClearsFlag proves `ai models enable` clears the disabled
// flag and starts the server with NO re-download.
func TestModelsEnableStartsAndClearsFlag(test *testing.T) {
	fakeHF := &hf.Fake{}
	withFakeHF(test, fakeHF)
	withFakeRegistrar(test, &fakeRegistrar{})
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "my-qwen", Model: "mlx-community/Qwen3-8B", Runtime: config.RuntimeVLLM,
		Endpoint: "http://127.0.0.1:8101/v1", Status: "registered", GPUMemoryUtilization: 0.5, Disabled: true,
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}
	server := &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"}
	withFakeVLLM(test, true, nil, server)

	exit := output.ExitOK
	cmd := newModelsEnableCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "my-qwen")
	if exit != output.ExitOK {
		test.Fatalf("enable exit = %d, want 0", exit)
	}
	if len(fakeHF.DownloadedAll) != 0 {
		test.Fatalf("Download called %d times, want 0 (enable must not re-pull weights)", len(fakeHF.DownloadedAll))
	}
	if len(server.opts) != 1 || server.opts[0].GPUMemoryUtilization != 0.5 {
		test.Fatalf("EnsureServedWithOptions opts = %+v, want the preserved GPUMemoryUtilization", server.opts)
	}
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || choice.Disabled {
		test.Fatalf("recorded choice = %+v (ok=%v), want Disabled=false", choice, ok)
	}
}

// TestModelsDisableUnknownModelExits2 proves an unrecorded model is rejected.
func TestModelsDisableUnknownModelExits2(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	withFakeVLLM(test, true, nil, &fakeVLLMServer{})
	exit := output.ExitOK
	cmd := newModelsDisableCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "never-pulled")
	if exit != output.ExitInvalidInput {
		test.Fatalf("exit = %d, want ExitInvalidInput for a model with no recorded vLLM runtime choice", exit)
	}
}

// TestModelsConfigurePreservesDisabledFlag proves re-running `ai models configure`
// on a disabled model does NOT silently re-enable it — recordModelRuntimeChoice must
// carry over the existing Disabled flag rather than resetting it to false.
func TestModelsConfigurePreservesDisabledFlag(test *testing.T) {
	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "my-qwen", Model: "mlx-community/Qwen3-8B", Runtime: config.RuntimeVLLM,
		Endpoint: "http://127.0.0.1:8101/v1", Status: "registered", Disabled: true,
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}
	withFakeVLLM(test, true, nil, &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"})

	exit := output.ExitOK
	cmd := newModelsConfigureCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, "--gpu-memory-utilization", "0.3", "my-qwen")
	if exit != output.ExitOK {
		test.Fatalf("configure exit = %d, want 0", exit)
	}
	choice, ok := config.ModelRuntimeFor("my-qwen")
	if !ok || !choice.Disabled {
		test.Fatalf("recorded choice = %+v (ok=%v), want Disabled to stay true", choice, ok)
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

// TestModelsPullVLLMToolCallParserWired proves `ai models pull` looks up the curated
// model's confirmed vLLM --tool-call-parser and threads it into EnsureServedWithToolParser
// — the fix for LiteLLM's "auto tool choice requires --enable-auto-tool-choice and
// --tool-call-parser to be set" error. It finds any curated repo for this OS that HAS
// a confirmed parser (so the test is not tied to one specific model or OS) and an
// uncurated repo, which must get an empty parser rather than a guess.
func TestModelsPullVLLMToolCallParserWired(test *testing.T) {
	var curatedRepo, curatedParser string
	for _, model := range hf.CuratedModels(goruntime.GOOS) {
		if model.ToolCallParser != "" {
			curatedRepo, curatedParser = model.Repo, model.ToolCallParser
			break
		}
	}
	if curatedRepo == "" {
		test.Skip("no curated model on this OS has a confirmed ToolCallParser")
	}

	withFakeHF(test, &hf.Fake{})
	withFakeRegistrar(test, &fakeRegistrar{})
	server := &fakeVLLMServer{port: 8101, endpoint: "http://127.0.0.1:8101/v1"}
	withFakeVLLM(test, true, nil, server)
	exit := output.ExitOK
	cmd := newModelsPullCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, curatedRepo, "mlx-community/not-a-curated-repo")
	if exit != output.ExitOK {
		test.Fatalf("pull exit = %d, want 0", exit)
	}
	if len(server.served) != 2 {
		test.Fatalf("served = %v, want 2 entries", server.served)
	}
	if want := vllmDefaultAlias(curatedRepo) + "|" + curatedRepo + "|" + curatedParser; server.served[0] != want {
		test.Errorf("served[0] = %q, want %q (curated parser must reach EnsureServedWithToolParser)", server.served[0], want)
	}
	if want := "not-a-curated-repo|mlx-community/not-a-curated-repo|"; server.served[1] != want {
		test.Errorf("served[1] = %q, want %q (an uncurated repo must get an EMPTY parser, never a guess)", server.served[1], want)
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

// A CacheRemove failure on the recorded-vLLM rm path must surface as exit 4 (matching
// the non-recorded rm path) and must NOT clear the runtime record — so the removal is
// retryable and `ai models show` doesn't report the model gone while its weights are
// still on disk.
func TestModelsRmVLLMCacheRemoveFailureExits4KeepsRecord(test *testing.T) {
	fake := &hf.Fake{RemoveErr: errors.New("permission denied")}
	withFakeHF(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	withFakeVLLM(test, true, nil, &fakeVLLMServer{})
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
	if exit != output.ExitRuntimeFailure {
		test.Fatalf("rm exit on CacheRemove failure = %d, want %d", exit, output.ExitRuntimeFailure)
	}
	if _, ok := config.ModelRuntimeFor("my-qwen"); !ok {
		test.Fatal("runtime choice must survive a failed weight deletion")
	}
	if len(registrar.vllmUnregistered) != 1 || registrar.vllmUnregistered[0] != "my-qwen" {
		test.Fatalf("vLLM unregistered = %v, want [my-qwen] (best-effort step still runs)", registrar.vllmUnregistered)
	}
}

// rm by REPO ID when the same repo was pulled under two different --alias values must
// unregister/stop BOTH recorded choices and clear both records — not just the first
// match — before the shared weights are deleted once.
func TestModelsRmVLLMDualAliasRemovesBoth(test *testing.T) {
	fake := &hf.Fake{}
	withFakeHF(test, fake)
	registrar := &fakeRegistrar{}
	withFakeRegistrar(test, registrar)
	stoppedPorts := withFakeVLLM(test, true, nil, &fakeVLLMServer{})
	const repo = "mlx-community/Qwen2.5-7B-Instruct-4bit"
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "alias-a", Model: repo, Runtime: config.RuntimeVLLM, Endpoint: "http://127.0.0.1:8101/v1",
	}); err != nil {
		test.Fatalf("seed alias-a: %v", err)
	}
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "alias-b", Model: repo, Runtime: config.RuntimeVLLM, Endpoint: "http://127.0.0.1:8102/v1",
	}); err != nil {
		test.Fatalf("seed alias-b: %v", err)
	}
	exit := output.ExitOK
	cmd := newModelsRmCmd(jsonEmitter(), &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	runLocalModelsCmd(test, cmd, repo)
	if exit != output.ExitOK {
		test.Fatalf("dual-alias rm exit = %d, want 0", exit)
	}
	slices.Sort(registrar.vllmUnregistered)
	if !slices.Equal(registrar.vllmUnregistered, []string{"alias-a", "alias-b"}) {
		test.Fatalf("vLLM unregistered = %v, want both aliases", registrar.vllmUnregistered)
	}
	slices.Sort(*stoppedPorts)
	if !slices.Equal(*stoppedPorts, []int{8101, 8102}) {
		test.Fatalf("StopByPort ports = %v, want [8101 8102]", *stoppedPorts)
	}
	if len(fake.RemovedAll) != 1 || fake.RemovedAll[0] != repo {
		test.Fatalf("hf cache rm calls = %v, want the shared repo removed exactly once", fake.RemovedAll)
	}
	if _, ok := config.ModelRuntimeFor("alias-a"); ok {
		test.Fatal("alias-a runtime choice should be cleared after rm")
	}
	if _, ok := config.ModelRuntimeFor("alias-b"); ok {
		test.Fatal("alias-b runtime choice should be cleared after rm")
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
