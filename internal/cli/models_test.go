package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

// fakeLitellmClient is an in-memory litellm.Client for the CLI status tests (no
// network). It returns the canned StatusInfo for Status(); Test/Models are unused
// by the status path but satisfy the interface.
type fakeLitellmClient struct {
	status litellm.StatusInfo
}

func (fake fakeLitellmClient) Status() (litellm.StatusInfo, error) { return fake.status, nil }
func (fakeLitellmClient) Test(string) (litellm.TestResult, error)  { return litellm.TestResult{}, nil }
func (fakeLitellmClient) Models() ([]litellm.Model, error)         { return nil, nil }

// runModelsStatusJSON drives `ai models status` with a JSON emitter, injecting the
// given gateway status via the litellmClient package var. It returns the exit code
// and the decoded envelope data (the StatusInfo).
func runModelsStatusJSON(test *testing.T, status litellm.StatusInfo) (int, litellm.StatusInfo) {
	test.Helper()
	original := litellmClient
	litellmClient = func() litellm.Client { return fakeLitellmClient{status: status} }
	defer func() { litellmClient = original }()

	var buf bytes.Buffer
	emitter := &output.Emitter{Out: &buf, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newModelsStatusCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("models status returned error: %v", err)
	}
	var envelope struct {
		Data litellm.StatusInfo `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		test.Fatalf("envelope not valid JSON: %v\n%s", err, buf.String())
	}
	return exit, envelope.Data
}

// The status command renders the LIVE served-model list + derived providers.
func TestModelsStatusRendersLiveModels(test *testing.T) {
	status := litellm.StatusInfo{
		Healthy:   true,
		Default:   "gemma4",
		Providers: []string{"anthropic", "vllm"},
		Local:     true,
		BaseURL:   "http://127.0.0.1:14000",
		Models: []litellm.Model{
			{Name: "vllm/gemma4", Provider: "openai", Mode: "chat"},
			{Name: "claude-opus", Provider: "anthropic", Mode: "chat"},
		},
	}
	exit, data := runModelsStatusJSON(test, status)
	if exit != output.ExitOK {
		test.Fatalf("exit = %d, want 0", exit)
	}
	if len(data.Models) != 2 {
		test.Fatalf("envelope served models = %d, want 2", len(data.Models))
	}
	if strings.Join(data.Providers, ",") != "anthropic,vllm" {
		test.Errorf("providers = %v, want the live-derived set", data.Providers)
	}
	// Human render shows the served list too.
	if rendered := data.Human(); !strings.Contains(rendered, "claude-opus") || !strings.Contains(rendered, "Served models") {
		test.Errorf("human render missing the served list:\n%s", rendered)
	}
}

// A reachable gateway whose model-list call failed must still report health (exit
// 0) with a note — not error out.
func TestModelsStatusModelListFailureStillShowsHealth(test *testing.T) {
	status := litellm.StatusInfo{
		Healthy:    true,
		Default:    "gemma4",
		BaseURL:    "http://127.0.0.1:14000",
		ModelsNote: "could not list served models: unauthorized",
	}
	exit, data := runModelsStatusJSON(test, status)
	if exit != output.ExitOK {
		test.Fatalf("exit = %d, want 0 (health still reported)", exit)
	}
	if !data.Healthy {
		test.Error("gateway should be reported healthy")
	}
	if data.ModelsNote == "" {
		test.Error("expected the model-list note carried through the envelope")
	}
}

// runModelsTest drives `ai models test [args...]` with a JSON emitter (so
// interactive() is false), returning the exit code.
func runModelsTest(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newModelsTestCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("models test %v returned error: %v", args, err)
	}
	return exit
}

// No model arg on a non-TTY must exit 2, not block on a prompt.
func TestModelsTestMissingModelNonInteractive(test *testing.T) {
	if exit := runModelsTest(test); exit != output.ExitInvalidInput {
		test.Fatalf("models test with no model: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// modelsListClient is a fake litellm.Client whose Models() returns a canned set,
// used to drive servedModelNames (the prompt's candidate source).
type modelsListClient struct {
	models []litellm.Model
	err    error
}

func (fake modelsListClient) Status() (litellm.StatusInfo, error) { return litellm.StatusInfo{}, nil }
func (modelsListClient) Test(string) (litellm.TestResult, error)  { return litellm.TestResult{}, nil }
func (fake modelsListClient) Models() ([]litellm.Model, error)    { return fake.models, fake.err }

// servedModelNames offers the LIVE served models (DB-backed), sorted, with any
// wildcard handle excluded — the named-alias routing was removed.
func TestServedModelNamesExcludeWildcards(test *testing.T) {
	original := litellmClient
	defer func() { litellmClient = original }()
	litellmClient = func() litellm.Client {
		return modelsListClient{models: []litellm.Model{
			{Name: "openai/gpt-5.5", Provider: "openai"},
			{Name: "vllm/gemma4", Provider: "openai"},
			{Name: "openai/*", Provider: "openai"}, // wildcard → excluded
		}}
	}
	got := servedModelNames()
	want := []string{"openai/gpt-5.5", "vllm/gemma4"} // sorted, wildcard dropped
	if strings.Join(got, ",") != strings.Join(want, ",") {
		test.Errorf("servedModelNames() = %v, want %v", got, want)
	}
}

// A down/empty gateway yields no candidates (the prompt then falls back to the seed).
func TestServedModelNamesGatewayDown(test *testing.T) {
	original := litellmClient
	defer func() { litellmClient = original }()
	litellmClient = func() litellm.Client {
		return modelsListClient{err: io.ErrUnexpectedEOF}
	}
	if got := servedModelNames(); len(got) != 0 {
		test.Errorf("servedModelNames() = %v, want empty when the gateway is down", got)
	}
}
