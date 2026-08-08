package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

// modelTestError picks exit 5 for auth/credential failures, exit 4 otherwise,
// and appends an actionable hint.
func TestModelTestError(test *testing.T) {
	var platformErr *output.Error

	// 401 → permission (exit 5) with a keys hint.
	authErr := modelTestError(litellm.TestResult{Model: "openai/gpt-5.5", Status: 401, Error: "unauthorized"})
	if !errors.As(authErr, &platformErr) || platformErr.Code != output.ExitPermission {
		test.Fatalf("401 should map to permission (exit 5): %v", authErr)
	}
	if !strings.Contains(authErr.Error(), "ai keys add") {
		test.Fatalf("auth error should hint at `ai keys add`: %v", authErr)
	}

	// 404 for a local vLLM model → runtime (exit 4) with a pull hint.
	notFound := modelTestError(litellm.TestResult{Model: "vllm/llama3", Status: 404, Error: "not found"})
	if !errors.As(notFound, &platformErr) || platformErr.Code != output.ExitRuntimeFailure {
		test.Fatalf("404 should map to runtime failure (exit 4): %v", notFound)
	}
	if !strings.Contains(notFound.Error(), "ai models pull llama3") {
		test.Fatalf("vllm 404 should hint at pulling: %v", notFound)
	}

	// A generic failure with no message still reports the HTTP status at exit 4.
	generic := modelTestError(litellm.TestResult{Model: "openai/gpt-5.5", Status: 500})
	if !errors.As(generic, &platformErr) || platformErr.Code != output.ExitRuntimeFailure {
		test.Fatalf("500 should map to runtime failure (exit 4): %v", generic)
	}
	if !strings.Contains(generic.Error(), "HTTP 500") {
		test.Fatalf("empty error should surface the HTTP status: %v", generic)
	}
}

func TestModelsRmResultHuman(test *testing.T) {
	clean := modelsRmResult{Model: "ollama/llama3"}.Human()
	if !strings.Contains(clean, "ollama/llama3") || !strings.Contains(clean, "removed") {
		test.Fatalf("clean rm Human missing fields:\n%s", clean)
	}
	withWarn := modelsRmResult{Model: "ollama/llama3", UnregisterError: "gateway down"}.Human()
	if !strings.Contains(withWarn, "gateway down") {
		test.Fatalf("rm Human should surface the unregister warning:\n%s", withWarn)
	}
}
