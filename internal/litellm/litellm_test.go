package litellm

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderDefaultRouting(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := Render(DefaultRouting(), ""); err != nil {
		test.Fatal(err)
	}
	p, _ := ConfigPath()
	b, err := os.ReadFile(p)
	if err != nil {
		test.Fatal(err)
	}
	var cfg struct {
		ModelList []struct {
			ModelName     string `yaml:"model_name"`
			LitellmParams struct {
				Model  string `yaml:"model"`
				APIKey string `yaml:"api_key"`
			} `yaml:"litellm_params"`
		} `yaml:"model_list"`
		LitellmSettings struct {
			DefaultModel string `yaml:"default_model"`
		} `yaml:"litellm_settings"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		test.Fatalf("rendered config is not valid yaml: %v\n%s", err, b)
	}
	if cfg.LitellmSettings.DefaultModel != "gpt-5" {
		test.Fatalf("default_model = %q, want gpt-5", cfg.LitellmSettings.DefaultModel)
	}
	byName := map[string]string{}
	keyByName := map[string]string{}
	for _, entry := range cfg.ModelList {
		byName[entry.ModelName] = entry.LitellmParams.Model
		keyByName[entry.ModelName] = entry.LitellmParams.APIKey
	}
	if byName["gpt-5"] != "openai/gpt-5" {
		test.Fatalf("gpt-5 -> %q", byName["gpt-5"])
	}
	// Credentials are placeholders only (never real values), arch §17.
	if keyByName["gpt-5"] != "os.environ/OPENAI_API_KEY" {
		test.Fatalf("gpt-5 api_key = %q, want placeholder", keyByName["gpt-5"])
	}
	// Ollama needs no credential.
	if keyByName["llama"] != "" {
		test.Fatalf("ollama alias should have no api_key, got %q", keyByName["llama"])
	}
}

func TestRenderProviderConfigPassthrough(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	src := filepath.Join(home, "mock-litellm.yaml")
	want := "model_list: [{model_name: gpt-5, litellm_params: {model: openai/gpt-5}}]\n"
	if err := os.WriteFile(src, []byte(want), 0o644); err != nil {
		test.Fatal(err)
	}
	if err := Render(DefaultRouting(), src); err != nil {
		test.Fatal(err)
	}
	p, _ := ConfigPath()
	got, _ := os.ReadFile(p)
	if string(got) != want {
		test.Fatalf("provider config not passed through verbatim:\n got %q\nwant %q", got, want)
	}
}

func TestProvidersAndOllama(test *testing.T) {
	routing := DefaultRouting()
	providers := providersOf(routing)
	if len(providers) == 0 {
		test.Fatal("expected providers")
	}
	if !hasOllama(routing) {
		test.Fatal("default routing includes an ollama alias")
	}
}
