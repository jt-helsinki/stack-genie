package output

import (
	"bytes"
	"encoding/json"
	"testing"
)

func newTestEmitter(json bool) (*Emitter, *bytes.Buffer, *bytes.Buffer) {
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	return &Emitter{JSON: json, Out: out, Err: errb}, out, errb
}

func TestSuccessJSONEnvelope(t *testing.T) {
	em, out, errb := newTestEmitter(true)
	code := em.Success("version", map[string]any{"version": "1.2.3"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr should be empty with --json, got %q", errb.String())
	}
	var env struct {
		OK       bool           `json:"ok"`
		Command  string         `json:"command"`
		Data     map[string]any `json:"data"`
		Error    *Error         `json:"error"`
		Warnings []string       `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out.String())
	}
	if !env.OK || env.Command != "version" || env.Error != nil {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	if env.Warnings == nil {
		t.Fatal("warnings must be an array, not null")
	}
	if env.Data["version"] != "1.2.3" {
		t.Fatalf("data.version = %v", env.Data["version"])
	}
}

func TestFailureCodeEqualsErrorCode(t *testing.T) {
	em, out, _ := newTestEmitter(true)
	code := em.Failure("project.create", Errorf(ExitInvalidInput, "bad name"))
	if code != ExitInvalidInput {
		t.Fatalf("exit = %d, want %d", code, ExitInvalidInput)
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error *Error `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if env.OK {
		t.Fatal("ok must be false on failure")
	}
	// error.code must equal the process exit code (§19).
	if env.Error.Code != code {
		t.Fatalf("error.code = %d, exit = %d; must match", env.Error.Code, code)
	}
	if env.Error.Kind != KindInvalidInput {
		t.Fatalf("kind = %q, want %q", env.Error.Kind, KindInvalidInput)
	}
}

func TestKindForCode(t *testing.T) {
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
			t.Errorf("kindForCode(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestHumanFailureWritesToStderrNotStdout(t *testing.T) {
	em, out, errb := newTestEmitter(false)
	em.Failure("doctor", Errorf(ExitMissingDep, "docker not found"))
	if out.Len() != 0 {
		t.Fatalf("stdout should be empty on human failure, got %q", out.String())
	}
	if errb.Len() == 0 {
		t.Fatal("human error should be written to stderr")
	}
}
