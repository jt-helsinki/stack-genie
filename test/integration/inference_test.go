//go:build integration

package integration

import (
	"os"
	"testing"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
)

// Group 5: Gateway inference.
//
// `ai models test` against the tiny local model drives a real chat completion
// through nginx → Headroom → LiteLLM → vLLM and asserts a non-error response.
// This is the only group that performs actual (local) inference; cloud inference
// is opt-in and gated by AIP_INTEGRATION_<PROVIDER>_KEY (registration-only by
// default).
func TestGroup05GatewayInference(test *testing.T) {
	requireStack(test)

	test.Run("local smollm chat completion through the gateway", func(test *testing.T) {
		if !waitForServedModel(test, localModel, 30*time.Second) {
			test.Skipf("%s not served by the gateway", localModel)
		}
		// A small local model is still slow on a cold start through the full proxy
		// chain — give it a generous timeout. A non-error result is the signal.
		env, code, stderr := run(test, "", 5*time.Minute, "models", "test", localModel)
		if !assertOK(test, env, code, "models.test") {
			test.Errorf("models test %s failed (exit %d):\nstderr:\n%s\nerror:%+v",
				localModel, code, stderr, env.Error)
			return
		}
		var result litellm.TestResult
		env.dataInto(test, &result)
		if !result.OK {
			test.Errorf("models test %s: gateway responded but call failed: status=%d err=%q",
				localModel, result.Status, result.Error)
		}
	})

	test.Run("optional cloud inference (gated)", func(test *testing.T) {
		// Off by default. Set AIP_INTEGRATION_<PROVIDER>_KEY (e.g.
		// AIP_INTEGRATION_OPENAI_KEY) to register a real key and run one cloud
		// chat completion. The provider/model pair is conservative + cheap.
		cases := []struct {
			env      string
			provider string
			model    string
		}{
			{"AIP_INTEGRATION_OPENAI_KEY", "openai", "openai/gpt-4o-mini"},
			{"AIP_INTEGRATION_ANTHROPIC_KEY", "anthropic", "anthropic/claude-3-5-haiku-latest"},
			{"AIP_INTEGRATION_GEMINI_KEY", "google", "gemini/gemini-1.5-flash"},
			{"AIP_INTEGRATION_GROQ_KEY", "groq", "groq/llama-3.1-8b-instant"},
		}
		ran := false
		for _, testCase := range cases {
			key := os.Getenv(testCase.env)
			if key == "" {
				continue
			}
			ran = true
			testCase := testCase
			test.Run(testCase.provider, func(test *testing.T) {
				test.Cleanup(func() {
					run(test, "", 90*time.Second, "keys", "remove", testCase.provider)
				})
				if env, code, stderr := run(test, "", 90*time.Second, "keys", "add", testCase.provider, "--value", key); !assertOK(test, env, code, "keys.add") {
					test.Fatalf("keys add %s failed (exit %d): %s", testCase.provider, code, stderr)
				}
				if !waitForServedModel(test, testCase.model, 30*time.Second) {
					test.Skipf("%s not served after keying %s", testCase.model, testCase.provider)
				}
				env, code, stderr := run(test, "", 2*time.Minute, "models", "test", testCase.model)
				if !assertOK(test, env, code, "models.test") {
					test.Errorf("cloud models test %s failed (exit %d):\nstderr:\n%s\nerror:%+v",
						testCase.model, code, stderr, env.Error)
				}
			})
		}
		if !ran {
			test.Skip("no AIP_INTEGRATION_<PROVIDER>_KEY set — cloud inference is registration-only by default")
		}
	})
}
