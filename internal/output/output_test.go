package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func newTestEmitter(json bool) (*Emitter, *bytes.Buffer, *bytes.Buffer) {
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	return &Emitter{JSON: json, Out: out, Err: errb}, out, errb
}

func TestSuccessJSONEnvelope(test *testing.T) {
	em, out, errb := newTestEmitter(true)
	code := em.Success("version", map[string]any{"version": "1.2.3"})
	if code != ExitOK {
		test.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if errb.Len() != 0 {
		test.Fatalf("stderr should be empty with --json, got %q", errb.String())
	}
	var env struct {
		OK       bool           `json:"ok"`
		Command  string         `json:"command"`
		Data     map[string]any `json:"data"`
		Error    *Error         `json:"error"`
		Warnings []string       `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		test.Fatalf("stdout is not valid JSON: %v\n%s", err, out.String())
	}
	if !env.OK || env.Command != "version" || env.Error != nil {
		test.Fatalf("unexpected envelope: %+v", env)
	}
	if env.Warnings == nil {
		test.Fatal("warnings must be an array, not null")
	}
	if env.Data["version"] != "1.2.3" {
		test.Fatalf("data.version = %v", env.Data["version"])
	}
}

func TestFailureCodeEqualsErrorCode(test *testing.T) {
	em, out, _ := newTestEmitter(true)
	code := em.Failure("project.create", Errorf(ExitInvalidInput, "bad name"))
	if code != ExitInvalidInput {
		test.Fatalf("exit = %d, want %d", code, ExitInvalidInput)
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error *Error `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		test.Fatalf("invalid JSON: %v", err)
	}
	if env.OK {
		test.Fatal("ok must be false on failure")
	}
	// error.code must equal the process exit code (§19).
	if env.Error.Code != code {
		test.Fatalf("error.code = %d, exit = %d; must match", env.Error.Code, code)
	}
	if env.Error.Kind != KindInvalidInput {
		test.Fatalf("kind = %q, want %q", env.Error.Kind, KindInvalidInput)
	}
}

func TestKindForCode(test *testing.T) {
	cases := map[int]string{
		ExitGeneral:        KindGeneral,
		ExitInvalidInput:   KindInvalidInput,
		ExitMissingDep:     KindMissingDep,
		ExitRuntimeFailure: KindRuntimeFailure,
		ExitPermission:     KindPermission,
		99:                 KindGeneral, // unknown codes fall back to general
	}
	for code, want := range cases {
		if got := kindForCode(code); got != want {
			test.Errorf("kindForCode(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestHumanFailureWritesToStderrNotStdout(test *testing.T) {
	em, out, errb := newTestEmitter(false)
	em.Failure("doctor", Errorf(ExitMissingDep, "docker not found"))
	if out.Len() != 0 {
		test.Fatalf("stdout should be empty on human failure, got %q", out.String())
	}
	if errb.Len() == 0 {
		test.Fatal("human error should be written to stderr")
	}
}

type humanData struct{ Name string }

func (humanData) Human() string { return "rendered: neat" }

func TestHumanRendererUsedWithoutJSON(test *testing.T) {
	em, out, _ := newTestEmitter(false)
	em.Success("x", humanData{Name: "ignored"})
	if got := out.String(); got != "rendered: neat\n" {
		test.Fatalf("Human() output = %q, want %q", got, "rendered: neat\n")
	}
}

func TestNonJSONFallsBackToYAMLNotJSON(test *testing.T) {
	em, out, _ := newTestEmitter(false)
	em.Success("x", map[string]any{"alpha": 1, "beta": "two"})
	got := out.String()
	if len(got) > 0 && got[0] == '{' {
		test.Fatalf("non-JSON output should not be JSON braces, got %q", got)
	}
	for _, fragment := range []string{"alpha: 1", "beta: two"} {
		if !bytes.Contains([]byte(got), []byte(fragment)) {
			test.Errorf("YAML output missing %q:\n%s", fragment, got)
		}
	}
}

func TestJSONFlagStillEmitsJSON(test *testing.T) {
	em, out, _ := newTestEmitter(true)
	em.Success("x", humanData{Name: "kept"})
	if got := out.String(); len(got) == 0 || got[0] != '{' {
		test.Fatalf("--json should emit a JSON envelope, got %q", got)
	}
}

// Error() must return the message verbatim (so *Error satisfies the error
// interface), and WithDetails must attach the payload AND return the same
// *Error so callers can chain output.Errorf(...).WithDetails(...).
func TestErrorMethodAndWithDetailsChaining(test *testing.T) {
	details := map[string]any{"missing": []string{"docker", "msb"}}
	platformErr := Errorf(ExitMissingDep, "need %d deps", 2).WithDetails(details)

	if platformErr.Error() != "need 2 deps" {
		test.Fatalf("Error() = %q, want %q", platformErr.Error(), "need 2 deps")
	}
	// WithDetails returns the SAME pointer (chainable), with Details set.
	if platformErr.Details == nil {
		test.Fatal("WithDetails did not attach the payload")
	}

	// The details ride through the JSON envelope on failure.
	em, out, _ := newTestEmitter(true)
	code := em.Failure("setup", platformErr)
	if code != ExitMissingDep {
		test.Fatalf("exit = %d, want %d", code, ExitMissingDep)
	}
	var env struct {
		Error struct {
			Kind    string `json:"kind"`
			Details struct {
				Missing []string `json:"missing"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		test.Fatalf("invalid JSON: %v", err)
	}
	if env.Error.Kind != KindMissingDep {
		test.Fatalf("kind = %q, want %q", env.Error.Kind, KindMissingDep)
	}
	if len(env.Error.Details.Missing) != 2 || env.Error.Details.Missing[0] != "docker" {
		test.Fatalf("details did not survive the envelope: %+v", env.Error.Details)
	}
}

// asError (exercised via Failure) coerces a nil or untyped error into a
// KindGeneral *Error with the general exit code, while an existing *Error is
// passed through with its own code.
func TestFailureCoercesNilAndUntypedErrors(test *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"nil error", nil, ExitGeneral, "unknown error"},
		{"untyped error", errors.New("disk full"), ExitGeneral, "disk full"},
		{"typed error keeps its code", Errorf(ExitPermission, "denied"), ExitPermission, "denied"},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			em, out, _ := newTestEmitter(true)
			code := em.Failure("cmd", testCase.err)
			if code != testCase.wantCode {
				test.Fatalf("exit = %d, want %d", code, testCase.wantCode)
			}
			var env struct {
				Error *Error `json:"error"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				test.Fatalf("invalid JSON: %v", err)
			}
			if env.Error.Message != testCase.wantMsg {
				test.Fatalf("message = %q, want %q", env.Error.Message, testCase.wantMsg)
			}
			// error.code must always equal the exit code (§19).
			if env.Error.Code != code {
				test.Fatalf("error.code = %d, exit = %d", env.Error.Code, code)
			}
		})
	}
}

// A human-mode success writes any warnings to stderr (never stdout) and, for
// nil data, produces no stdout at all.
func TestHumanSuccessRendersWarningsAndSkipsNilData(test *testing.T) {
	em, out, errb := newTestEmitter(false)
	code := em.Success("cmd", nil, "cache stale", "using fallback")
	if code != ExitOK {
		test.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if out.Len() != 0 {
		test.Fatalf("nil data must not write to stdout, got %q", out.String())
	}
	for _, want := range []string{"warning: cache stale", "warning: using fallback"} {
		if !bytes.Contains(errb.Bytes(), []byte(want)) {
			test.Errorf("stderr missing %q:\n%s", want, errb.String())
		}
	}
}

// A plain string payload is printed as-is (with a trailing newline) in human
// mode — distinct from the YAML fallback used for structured data.
func TestHumanStringPayloadPrintedVerbatim(test *testing.T) {
	em, out, _ := newTestEmitter(false)
	em.Success("cmd", "just text")
	if got := out.String(); got != "just text\n" {
		test.Fatalf("string payload = %q, want %q", got, "just text\n")
	}
}
