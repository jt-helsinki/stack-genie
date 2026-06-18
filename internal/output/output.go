// Package output renders the single JSON envelope (CLI spec §19) and maps
// platform errors to process exit codes (CLI spec §18).
//
// With --json, the envelope is the ONLY thing written to stdout; all
// diagnostics go to stderr.
package output

import (
	"encoding/json"
	"fmt"
	"io"
)

// Process exit codes (CLI spec §18).
const (
	ExitOK             = 0
	ExitGeneral        = 1
	ExitInvalidInput   = 2
	ExitMissingDep     = 3
	ExitRuntimeFailure = 4
	ExitPermission     = 5
)

// Error kinds (CLI spec §19) — one canonical kind per non-zero exit code.
const (
	KindGeneral        = "general_error"
	KindInvalidInput   = "invalid_input"
	KindMissingDep     = "missing_dependency"
	KindRuntimeFailure = "runtime_failure"
	KindPermission     = "permission_denied"
)

// kindForCode returns the canonical kind for an exit code.
func kindForCode(code int) string {
	switch code {
	case ExitInvalidInput:
		return KindInvalidInput
	case ExitMissingDep:
		return KindMissingDep
	case ExitRuntimeFailure:
		return KindRuntimeFailure
	case ExitPermission:
		return KindPermission
	default:
		return KindGeneral
	}
}

// Error is a platform error that carries the exit code it maps to. Its Code
// field is emitted as error.code in the envelope and equals the process exit
// code (CLI spec §19).
type Error struct {
	Code    int    `json:"code"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Errorf builds an *Error for the given exit code; the kind is derived from it.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Kind: kindForCode(code), Message: fmt.Sprintf(format, args...)}
}

// envelope is the wire shape emitted with --json (CLI spec §19).
type envelope struct {
	OK       bool     `json:"ok"`
	Command  string   `json:"command"`
	Data     any      `json:"data"`
	Error    *Error   `json:"error"`
	Warnings []string `json:"warnings"`
}

// Emitter renders command results as either the JSON envelope or human text.
type Emitter struct {
	JSON bool
	Out  io.Writer // stdout — envelope only when JSON
	Err  io.Writer // stderr — diagnostics, warnings, human errors
}

// Success renders a successful result and returns ExitOK.
func (e *Emitter) Success(command string, data any, warnings ...string) int {
	w := normalize(warnings)
	if e.JSON {
		e.writeJSON(envelope{OK: true, Command: command, Data: data, Error: nil, Warnings: w})
	} else {
		for _, msg := range w {
			_, _ = fmt.Fprintln(e.Err, "warning:", msg)
		}
		e.writeHuman(data)
	}
	return ExitOK
}

// Failure renders an error result and returns its exit code.
func (e *Emitter) Failure(command string, err error, warnings ...string) int {
	cerr := asError(err)
	w := normalize(warnings)
	if e.JSON {
		e.writeJSON(envelope{OK: false, Command: command, Data: nil, Error: cerr, Warnings: w})
	} else {
		for _, msg := range w {
			_, _ = fmt.Fprintln(e.Err, "warning:", msg)
		}
		_, _ = fmt.Fprintf(e.Err, "error: %s\n", cerr.Message)
	}
	return cerr.Code
}

// asError coerces any error into an *Error (general failure if untyped).
func asError(err error) *Error {
	if err == nil {
		return Errorf(ExitGeneral, "unknown error")
	}
	if ce, ok := err.(*Error); ok {
		return ce
	}
	return Errorf(ExitGeneral, "%s", err.Error())
}

func normalize(w []string) []string {
	if w == nil {
		return []string{}
	}
	return w
}

func (e *Emitter) writeJSON(env envelope) {
	enc := json.NewEncoder(e.Out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(env) // Encode appends a trailing newline
}

func (e *Emitter) writeHuman(data any) {
	switch v := data.(type) {
	case nil:
		// nothing to print
	case string:
		_, _ = fmt.Fprintln(e.Out, v)
	default:
		b, _ := json.MarshalIndent(v, "", "  ")
		_, _ = fmt.Fprintln(e.Out, string(b))
	}
}
