package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/secrets"
)

// secretsResult.Human() renders the table headers and a sample row — names and
// the bound env var only, never values.
func TestSecretsResultHuman(test *testing.T) {
	result := secretsResult{Secrets: []secrets.Entry{
		{Name: "OPENAI_API_KEY", EnvVar: "OPENAI_API_KEY"},
		{Name: "ANTHROPIC_API_KEY"},
	}}
	rendered := result.Human()
	for _, want := range []string{"NAME", "ENV-VAR", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("secrets list table missing %q:\n%s", want, rendered)
		}
	}
	if !strings.Contains(rendered, "—") {
		test.Errorf("unbound credential should render an em dash:\n%s", rendered)
	}
}

// The friendly empty case is shown when no credentials are stored.
func TestSecretsResultHumanEmpty(test *testing.T) {
	rendered := secretsResult{}.Human()
	if !strings.Contains(rendered, "No credentials yet") || !strings.Contains(rendered, "ai secrets set") {
		test.Errorf("empty secrets list hint missing:\n%s", rendered)
	}
}

// The JSON envelope still nests the entries under the `secrets` key.
func TestSecretsResultJSONShape(test *testing.T) {
	out := &bytes.Buffer{}
	emitter := &output.Emitter{JSON: true, Out: out, Err: &bytes.Buffer{}}
	emitter.Success("secrets.list", secretsResult{Secrets: []secrets.Entry{{Name: "OPENAI_API_KEY"}}})

	var envelope struct {
		Data struct {
			Secrets []struct {
				Name string `json:"name"`
			} `json:"secrets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		test.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(envelope.Data.Secrets) != 1 || envelope.Data.Secrets[0].Name != "OPENAI_API_KEY" {
		test.Fatalf("data.secrets shape changed: %+v", envelope.Data.Secrets)
	}
}
