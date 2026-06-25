package agentcfg

import (
	_ "embed"
	"sort"

	"gopkg.in/yaml.v3"
)

// cloudModelsYAML is the maintainer-curated seed of cloud model handles offered
// by the in-workspace agent CLIs' model picker, embedded at build time. It lists
// well-known current `<provider>/<model-id>` routes across openai/anthropic/
// gemini/groq. LiteLLM's per-provider wildcards route whatever an agent names, so
// this file is a convenience seed only — edit cloud_models.yaml to add/remove
// suggestions; no code change is needed.
//
//go:embed cloud_models.yaml
var cloudModelsYAML []byte

// cloudModelsDoc is the on-disk shape of cloud_models.yaml.
type cloudModelsDoc struct {
	Models []string `yaml:"models"`
}

// CloudModels parses the embedded curated cloud-models seed and returns the model
// routes, deduped and sorted for a deterministic config. A malformed embedded file
// would be a build-time maintainer error caught by the package's tests; at runtime
// a parse failure yields an empty list rather than failing the workspace start.
func CloudModels() []string {
	var doc cloudModelsDoc
	if err := yaml.Unmarshal(cloudModelsYAML, &doc); err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(doc.Models))
	models := make([]string, 0, len(doc.Models))
	for _, model := range doc.Models {
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}
