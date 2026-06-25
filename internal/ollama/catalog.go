package ollama

// CatalogModel is one installable model the platform suggests for `ai models
// pull` (the select list) and marks as "available" in `ai models list` when it is
// not yet in the local store. It is convenience data only — the pull path always
// also accepts a free-text custom reference, so an entry going stale never blocks
// anyone.
type CatalogModel struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Default     bool   `json:"default,omitempty"`
}

// Catalog returns the platform-curated list of popular, installable Ollama models
// (verified against ollama.com/library). This is convenience data — it WILL age as
// the library moves on, and that is fine: `ai models pull` always also offers an
// "enter a custom model…" path, so any current or future tag (including custom
// refs like hf.co/user/model) can still be pulled even when it is not listed here.
//
// The platform's default Ollama model (litellm.DefaultRouting's gemma4, served as
// ollama/gemma4:31b) is included and marked Default so `ai models pull` highlights
// it. We list the bare gemma4 tag (not :31b) so a default `ollama pull gemma4`
// resolves the library's recommended tag.
func Catalog() []CatalogModel {
	return []CatalogModel{
		{Name: "gemma4", Description: "Google Gemma 4 — the platform default local model", Default: true},
		{Name: "gemma3", Description: "Google Gemma 3 (270M–27B), with vision support"},
		{Name: "llama3.2", Description: "Meta Llama 3.2 — compact 1B/3B general model"},
		{Name: "llama3.1", Description: "Meta Llama 3.1 (8B default) — capable general model"},
		{Name: "llama3.3", Description: "Meta Llama 3.3 70B — high-end general model"},
		{Name: "qwen2.5", Description: "Alibaba Qwen2.5 — strong general model, 128K context"},
		{Name: "qwen2.5-coder", Description: "Qwen2.5 Coder — code-specialised model"},
		{Name: "mistral", Description: "Mistral 7B — fast, capable foundational model"},
		{Name: "phi4", Description: "Microsoft Phi-4 14B — strong small reasoning model"},
		{Name: "phi3.5", Description: "Microsoft Phi-3.5 3.8B — lightweight performer"},
		{Name: "deepseek-r1", Description: "DeepSeek-R1 — open reasoning model family"},
		{Name: "deepseek-coder-v2", Description: "DeepSeek-Coder-V2 — code-specialised MoE model"},
	}
}

// DefaultCatalogModel returns the name of the platform's default installable model
// (the one Catalog marks Default), or "" if none is marked.
func DefaultCatalogModel() string {
	for _, entry := range Catalog() {
		if entry.Default {
			return entry.Name
		}
	}
	return ""
}
