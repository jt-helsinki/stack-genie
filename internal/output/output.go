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
	"strings"

	"gopkg.in/yaml.v3"
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
	// Details optionally carries a machine-readable payload for the error (e.g.
	// the list of missing prerequisites from `ai setup`). Omitted when nil.
	Details any `json:"details,omitempty"`
}

func (platformErr *Error) Error() string { return platformErr.Message }

// WithDetails attaches a machine-readable payload to the error and returns it,
// so callers can chain: output.Errorf(code, msg).WithDetails(list).
func (platformErr *Error) WithDetails(details any) *Error {
	platformErr.Details = details
	return platformErr
}

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
func (emitter *Emitter) Success(command string, data any, warnings ...string) int {
	normalized := normalizeWarnings(warnings)
	if emitter.JSON {
		emitter.writeJSON(envelope{OK: true, Command: command, Data: data, Error: nil, Warnings: normalized})
	} else {
		for _, message := range normalized {
			_, _ = fmt.Fprintln(emitter.Err, "warning:", message)
		}
		emitter.writeHuman(data)
	}
	return ExitOK
}

// Failure renders an error result and returns its exit code.
func (emitter *Emitter) Failure(command string, err error, warnings ...string) int {
	platformErr := asError(err)
	normalized := normalizeWarnings(warnings)
	if emitter.JSON {
		emitter.writeJSON(envelope{OK: false, Command: command, Data: nil, Error: platformErr, Warnings: normalized})
	} else {
		for _, message := range normalized {
			_, _ = fmt.Fprintln(emitter.Err, "warning:", message)
		}
		_, _ = fmt.Fprintf(emitter.Err, "error: %s\n", platformErr.Message)
	}
	return platformErr.Code
}

// asError coerces any error into an *Error (general failure if untyped).
func asError(err error) *Error {
	if err == nil {
		return Errorf(ExitGeneral, "unknown error")
	}
	if platformErr, ok := err.(*Error); ok {
		return platformErr
	}
	return Errorf(ExitGeneral, "%s", err.Error())
}

func normalizeWarnings(warnings []string) []string {
	if warnings == nil {
		return []string{}
	}
	return warnings
}

func (emitter *Emitter) writeJSON(env envelope) {
	encoder := json.NewEncoder(emitter.Out)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(env) // Encode appends a trailing newline
}

// Human is implemented by a data payload that knows how to render itself for
// non-JSON output. writeHuman prefers it; otherwise it falls back to a readable
// YAML rendering. JSON is emitted only when --json is set.
type Human interface{ Human() string }

func (emitter *Emitter) writeHuman(data any) {
	switch value := data.(type) {
	case nil:
		// nothing to print
	case Human:
		if text := strings.TrimRight(value.Human(), "\n"); text != "" {
			_, _ = fmt.Fprintln(emitter.Out, text)
		}
	case string:
		_, _ = fmt.Fprintln(emitter.Out, value)
	default:
		encoded, err := yaml.Marshal(value)
		if err != nil {
			_, _ = fmt.Fprintf(emitter.Out, "%v\n", value)
			return
		}
		_, _ = fmt.Fprint(emitter.Out, string(encoded))
	}
}
