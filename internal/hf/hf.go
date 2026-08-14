// Package hf is the Hugging Face CLI (`hf`) integration layer — the platform's
// model-management backend now that vLLM is the SOLE local-inference runtime.
// It resolves the `hf` binary from the platform-managed host venv
// (~/.ai-platform/venv, internal/pyenv) exactly like internal/vllm resolves `vllm`,
// installs it into that venv, downloads model repos into the shared vLLM weight store
// (~/.ai-platform/volumes/models/vllm, so vLLM's HF_HOME finds them at serve time),
// and lists/removes locally-cached repos.
//
// It also ships the CURATED available-models list — a small in-repo set of
// vLLM-servable repos (mlx-community/* on darwin, plain HF repos on Linux).
// There is NO live HF Hub search.
//
// The exec seams (BinaryPath / Detect / the RealClient's download+cache calls) are
// injectable so the package is unit-tested without a real `hf` binary or any network,
// and the real host exec is `hardware bring-up` (it downloads multi-GB weights, which
// only happens on a provisioned host).
package hf

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/pyenv"
)

// pipInstallPackage is the pip requirement that provides the `hf` CLI. The `[cli]`
// extra installs the command-line entry point (`pip install -U "huggingface_hub[cli]"`).
const pipInstallPackage = "huggingface_hub[cli]"

// storeSubdirs is the vLLM weight store, kept BYTE-IDENTICAL to internal/vllm's
// StoreDir (~/.ai-platform/volumes/models/vllm). hf downloads point HF_HOME here so
// vLLM (which also sets HF_HOME to this dir at serve time) finds the weights. Declared
// here rather than imported from internal/vllm to avoid a package cycle; the two MUST
// stay in sync.
var storeSubdirs = []string{"models", "vllm"}

// lookPath locates a host binary. Injectable seam for tests.
var lookPath = exec.LookPath

// StoreDir is ~/.ai-platform/volumes/models/vllm — the shared vLLM weight store hf
// downloads into (via HF_HOME) and lists/removes from. Created-on-use (MkdirAll). It
// mirrors vllm.StoreDir so a `hf download` lands where `vllm serve` looks.
func StoreDir() (string, error) {
	volumesDir, err := paths.VolumesDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(append([]string{volumesDir}, storeSubdirs...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// BinaryPath resolves the `hf` executable the platform runs: the platform-managed
// venv's hf (~/.ai-platform/venv/bin/hf, pyenv.BinPath) when that file exists, else
// the bare name "hf" for a PATH lookup. Mirrors vllm.BinaryPath so once `hf` is
// installed into the venv (by Install / `ai setup`), Detect and the RealClient pick it
// up automatically. An unresolvable venv path degrades to "hf".
func BinaryPath() string {
	venvBinary, err := pyenv.BinPath("hf")
	if err != nil {
		return "hf"
	}
	if _, statErr := os.Stat(venvBinary); statErr == nil {
		return venvBinary
	}
	return "hf"
}

// Detect reports whether a usable `hf` CLI is present (resolvable on PATH or in the
// platform venv). A read-only probe — it only checks that the binary resolves.
func Detect() bool {
	_, err := lookPath(BinaryPath())
	return err == nil
}

// pyenv seams (package vars) so Install is unit-tested without a real toolchain/network.
var (
	pyenvEnsureFn     = pyenv.Ensure
	pyenvPipInstallFn = pyenv.PipInstall
)

// Install installs (or upgrades) the `hf` CLI into the platform-managed host venv
// (~/.ai-platform/venv): it ensures the venv exists (pyenv.Ensure) then pip-installs
// huggingface_hub[cli]. `ai setup` calls this best-effort (Detect-gated) so model
// management works after a plain setup, and it never fails setup on error. The real
// pip download is `hardware bring-up` behind the injectable pyenv seams.
func Install() error {
	if _, err := pyenvEnsureFn(); err != nil {
		return err
	}
	return pyenvPipInstallFn("-U", pipInstallPackage)
}

// CuratedModel is one entry in the curated available-models list: a vLLM-servable
// Hugging Face repo. Name is a short display handle (also the default gateway alias),
// Repo is the full HF repo id passed to `hf download` / `vllm serve`, Description is a
// one-liner, and Size is an approximate on-disk weight size.
type CuratedModel struct {
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	Description string `json:"description,omitempty"`
	Size        string `json:"size,omitempty"`
}

// The two curated lists below are HAND-CURATED — there is no auto-refresh and no live
// HF Hub search. They are kept model-for-model ALIGNED (same models, same order): entry
// N of curatedMLX is the mlx-community/* 4-bit build of the SAME model as entry N of
// curatedSafetensors (the GPU/safetensors build). macOS surfaces only the MLX list,
// Linux only the safetensors list (see CuratedModels). To extend: add the model to BOTH
// slices at the same index, verify each repo id resolves on https://huggingface.co
// (both the mlx-community/<x> id and the GPU id — do NOT invent ids), note when the GPU
// repo is GATED (needs `hf` login / license click-through — meta-llama/*), and give an
// approximate on-disk size. The set is ordered as a rough size ladder (small → large).
//
// Curated afresh from the live Hugging Face API as of 2026-08 (trending +
// most-downloaded text-generation, cross-referenced with the mlx-community builds).
// Every repo id below was verified to return HTTP 200 from GET /api/models/<repo>. The
// mlx-community/* mirrors are ungated even where the upstream GPU repo is gated; today
// only the two meta-llama/* GPU repos are gated (marked in the Description). Superseded
// families from the previous curation (Qwen2.5, Llama 3.3, Gemma 2/3, Mistral v0.3 /
// Mixtral / Nemo, Phi-3.5/4, DeepSeek-R1-Distill) were dropped for their current
// successors (Qwen3 / Qwen3.5 / Qwen3.6 / Qwen3-Coder, gpt-oss, Gemma 4, Devstral 2 /
// Ministral / Mistral-Small 3.1, DeepSeek-V4-Flash + R1-0528, Nemotron-3.5, LFM2.5).

// curatedMLX is the Apple-Silicon (darwin) curated set: mlx-community/* repos (4-bit
// unless the family ships a native low-bit format) served on the Metal GPU via the
// vLLM-Metal plugin.
var curatedMLX = []CuratedModel{
	// Tiny / small dense
	{Name: "Qwen3-0.6B-4bit", Repo: "mlx-community/Qwen3-0.6B-4bit", Description: "Qwen3 0.6B, 4-bit — tiny, fast", Size: "~0.4 GB"},
	{Name: "Qwen3.5-2B-MLX-4bit", Repo: "mlx-community/Qwen3.5-2B-MLX-4bit", Description: "Qwen3.5 2B, 4-bit — small general", Size: "~1.3 GB"},
	{Name: "LFM2.5-2.6B-4bit", Repo: "mlx-community/LFM2.5-2.6B-4bit", Description: "Liquid LFM2.5 2.6B, 4-bit — edge/on-device", Size: "~1.5 GB"},
	{Name: "Llama-3.2-3B-Instruct-4bit", Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit", Description: "Llama 3.2 3B instruct, 4-bit — small & fast", Size: "~1.8 GB"},
	{Name: "Qwen3.5-4B-MLX-4bit", Repo: "mlx-community/Qwen3.5-4B-MLX-4bit", Description: "Qwen3.5 4B, 4-bit — general", Size: "~2.3 GB"},
	{Name: "gemma-4-E4B-it-4bit", Repo: "mlx-community/gemma-4-e4b-it-4bit", Description: "Google Gemma 4 E4B instruct, 4-bit — compact multimodal", Size: "~4.5 GB"},
	// Mid dense (8–14B)
	{Name: "Meta-Llama-3.1-8B-Instruct-4bit", Repo: "mlx-community/Meta-Llama-3.1-8B-Instruct-4bit", Description: "Llama 3.1 8B instruct, 4-bit", Size: "~4.5 GB"},
	{Name: "DeepSeek-R1-0528-Qwen3-8B-4bit", Repo: "mlx-community/DeepSeek-R1-0528-Qwen3-8B-4bit", Description: "DeepSeek-R1-0528 distill (Qwen3 8B), 4-bit — reasoning", Size: "~4.6 GB"},
	{Name: "Qwen3.5-9B-MLX-4bit", Repo: "mlx-community/Qwen3.5-9B-MLX-4bit", Description: "Qwen3.5 9B, 4-bit — general", Size: "~5.0 GB"},
	{Name: "gemma-4-12B-it-4bit", Repo: "mlx-community/gemma-4-12B-it-qat-4bit", Description: "Google Gemma 4 12B instruct, QAT 4-bit — multimodal", Size: "~6.8 GB"},
	{Name: "Mellum2-12B-A2.5B-Instruct-4bit", Repo: "mlx-community/Mellum2-12B-A2.5B-Instruct-4bit", Description: "JetBrains Mellum2 12B-A2.5B, 4-bit — MoE code model", Size: "~6.8 GB"},
	// Large dense + MoE (20–36B)
	{Name: "gpt-oss-20b-MXFP4-Q8", Repo: "mlx-community/gpt-oss-20b-MXFP4-Q8", Description: "OpenAI gpt-oss 20B, MXFP4 — MoE, reasoning", Size: "~13 GB"},
	{Name: "Devstral-Small-2-24B-Instruct-2512-4bit", Repo: "mlx-community/Devstral-Small-2-24B-Instruct-2512-4bit", Description: "Mistral Devstral Small 2 24B, 4-bit — agentic coding", Size: "~13 GB"},
	{Name: "Mistral-Small-3.1-24B-Instruct-2503-4bit", Repo: "mlx-community/Mistral-Small-3.1-24B-Instruct-2503-4bit", Description: "Mistral Small 3.1 24B instruct, 4-bit — multimodal", Size: "~13 GB"},
	{Name: "Qwen3.6-27B-4bit", Repo: "mlx-community/Qwen3.6-27B-4bit", Description: "Qwen3.6 27B, 4-bit — flagship dense", Size: "~15 GB"},
	{Name: "Qwen3-Coder-30B-A3B-Instruct-4bit", Repo: "mlx-community/Qwen3-Coder-30B-A3B-Instruct-4bit", Description: "Qwen3-Coder 30B-A3B, 4-bit — MoE, top open coder", Size: "~17 GB"},
	{Name: "Qwen3-30B-A3B-Instruct-2507-4bit", Repo: "mlx-community/Qwen3-30B-A3B-Instruct-2507-4bit", Description: "Qwen3 30B-A3B (2507), 4-bit — MoE (3B active)", Size: "~17 GB"},
	{Name: "NVIDIA-Nemotron-3.5-Lightning-30B-A3B-4bit", Repo: "mlx-community/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-4bit", Description: "NVIDIA Nemotron 3.5 Lightning 30B-A3B, 4-bit — MoE reasoning", Size: "~17 GB"},
	{Name: "gemma-4-31B-it-4bit", Repo: "mlx-community/gemma-4-31b-it-4bit", Description: "Google Gemma 4 31B instruct, 4-bit — multimodal flagship", Size: "~17 GB"},
	{Name: "Qwen3.6-35B-A3B-4bit", Repo: "mlx-community/Qwen3.6-35B-A3B-4bit", Description: "Qwen3.6 35B-A3B, 4-bit — MoE flagship", Size: "~20 GB"},
	// Frontier MoE
	{Name: "gpt-oss-120b-MXFP4-Q8", Repo: "mlx-community/gpt-oss-120b-MXFP4-Q8", Description: "OpenAI gpt-oss 120B, MXFP4 — frontier MoE, reasoning", Size: "~63 GB"},
	{Name: "DeepSeek-V4-Flash-4bit", Repo: "mlx-community/DeepSeek-V4-Flash-4bit", Description: "DeepSeek-V4-Flash, 4-bit — 291B MoE flagship (very large)", Size: "~145 GB"},
}

// curatedSafetensors is the Linux/CUDA curated set: plain Hugging Face safetensors repos
// served on NVIDIA GPUs. Model-for-model aligned with curatedMLX (same index = same
// model). Sizes are the native weights (bf16/fp16, or MXFP4 for gpt-oss). "(gated repo)"
// marks repos needing `hf` login / license acceptance.
var curatedSafetensors = []CuratedModel{
	// Tiny / small dense
	{Name: "Qwen3-0.6B", Repo: "Qwen/Qwen3-0.6B", Description: "Qwen3 0.6B — tiny, fast", Size: "~1.5 GB"},
	{Name: "Qwen3.5-2B", Repo: "Qwen/Qwen3.5-2B", Description: "Qwen3.5 2B — small general", Size: "~4.5 GB"},
	{Name: "LFM2.5-2.6B", Repo: "LiquidAI/LFM2.5-2.6B", Description: "Liquid LFM2.5 2.6B — edge/on-device", Size: "~5.4 GB"},
	{Name: "Llama-3.2-3B-Instruct", Repo: "meta-llama/Llama-3.2-3B-Instruct", Description: "Llama 3.2 3B instruct — small & fast — gated, run `ai models login`", Size: "~6.5 GB"},
	{Name: "Qwen3.5-4B", Repo: "Qwen/Qwen3.5-4B", Description: "Qwen3.5 4B — general", Size: "~8 GB"},
	{Name: "gemma-4-E4B-it", Repo: "google/gemma-4-E4B-it", Description: "Google Gemma 4 E4B instruct — compact multimodal", Size: "~16 GB"},
	// Mid dense (8–14B)
	{Name: "Llama-3.1-8B-Instruct", Repo: "meta-llama/Llama-3.1-8B-Instruct", Description: "Llama 3.1 8B instruct — gated, run `ai models login`", Size: "~16 GB"},
	{Name: "DeepSeek-R1-0528-Qwen3-8B", Repo: "deepseek-ai/DeepSeek-R1-0528-Qwen3-8B", Description: "DeepSeek-R1-0528 distill (Qwen3 8B) — reasoning", Size: "~16 GB"},
	{Name: "Qwen3.5-9B", Repo: "Qwen/Qwen3.5-9B", Description: "Qwen3.5 9B — general", Size: "~18 GB"},
	{Name: "gemma-4-12B-it", Repo: "google/gemma-4-12B-it", Description: "Google Gemma 4 12B instruct — multimodal", Size: "~24 GB"},
	{Name: "Mellum2-12B-A2.5B-Instruct", Repo: "JetBrains/Mellum2-12B-A2.5B-Instruct", Description: "JetBrains Mellum2 12B-A2.5B — MoE code model", Size: "~24 GB"},
	// Large dense + MoE (20–36B)
	{Name: "gpt-oss-20b", Repo: "openai/gpt-oss-20b", Description: "OpenAI gpt-oss 20B — MoE, reasoning (MXFP4)", Size: "~13 GB"},
	{Name: "Devstral-Small-2-24B-Instruct-2512", Repo: "mistralai/Devstral-Small-2-24B-Instruct-2512", Description: "Mistral Devstral Small 2 24B — agentic coding", Size: "~47 GB"},
	{Name: "Mistral-Small-3.1-24B-Instruct-2503", Repo: "mistralai/Mistral-Small-3.1-24B-Instruct-2503", Description: "Mistral Small 3.1 24B instruct — multimodal", Size: "~47 GB"},
	{Name: "Qwen3.6-27B", Repo: "Qwen/Qwen3.6-27B", Description: "Qwen3.6 27B — flagship dense", Size: "~54 GB"},
	{Name: "Qwen3-Coder-30B-A3B-Instruct", Repo: "Qwen/Qwen3-Coder-30B-A3B-Instruct", Description: "Qwen3-Coder 30B-A3B — MoE, top open coder", Size: "~61 GB"},
	{Name: "Qwen3-30B-A3B-Instruct-2507", Repo: "Qwen/Qwen3-30B-A3B-Instruct-2507", Description: "Qwen3 30B-A3B (2507) — MoE (3B active)", Size: "~61 GB"},
	{Name: "NVIDIA-Nemotron-3.5-Lightning-30B-A3B-BF16", Repo: "nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-BF16", Description: "NVIDIA Nemotron 3.5 Lightning 30B-A3B — MoE reasoning", Size: "~63 GB"},
	{Name: "gemma-4-31B-it", Repo: "google/gemma-4-31B-it", Description: "Google Gemma 4 31B instruct — multimodal flagship", Size: "~62 GB"},
	{Name: "Qwen3.6-35B-A3B", Repo: "Qwen/Qwen3.6-35B-A3B", Description: "Qwen3.6 35B-A3B — MoE flagship", Size: "~72 GB"},
	// Frontier MoE
	{Name: "gpt-oss-120b", Repo: "openai/gpt-oss-120b", Description: "OpenAI gpt-oss 120B — frontier MoE, reasoning (MXFP4)", Size: "~63 GB"},
	{Name: "DeepSeek-V4-Flash", Repo: "deepseek-ai/DeepSeek-V4-Flash", Description: "DeepSeek-V4-Flash — 291B MoE flagship (very large)", Size: "~582 GB"},
}

// CuratedModels returns the curated available-models list for goos: the MLX set on
// darwin (Apple Silicon), the safetensors set elsewhere (Linux/CUDA). This is the
// installable set surfaced by `ai models popular` and the TUI Local Models "Available"
// section. The returned slice is a copy,
// safe for the caller to mutate.
func CuratedModels(goos string) []CuratedModel {
	source := curatedSafetensors
	if goos == "darwin" {
		source = curatedMLX
	}
	out := make([]CuratedModel, len(source))
	copy(out, source)
	return out
}

// DefaultCuratedModels returns the curated list for the current host OS.
func DefaultCuratedModels() []CuratedModel {
	return CuratedModels(runtime.GOOS)
}

// CachedModel is one locally-downloaded repo, parsed from `hf cache ls`. Repo is the
// HF repo id (the delete key); Size is the human size when the listing reports it.
type CachedModel struct {
	Repo string `json:"repo"`
	Size string `json:"size,omitempty"`
}

// Client manages the local HF weight cache (`ai models list|pull|rm`). The real impl
// shells out to `hf` with HF_HOME pointed at the vLLM store; tests use Fake. Every
// method returns a clean error so the CLI can map it to an exit code.
type Client interface {
	// Download fetches a repo's weights into the vLLM store (`hf download <repo>` with
	// HF_HOME set). progress receives raw output lines (best-effort — nil is tolerated).
	Download(repo string, progress func(line string)) error
	// CacheList returns the locally-downloaded repos (`hf cache ls`).
	CacheList() ([]CachedModel, error)
	// CacheRemove deletes a repo from the local cache (`hf cache rm <repo>`).
	CacheRemove(repo string) error
	// Login authenticates `hf` with the given token (`hf auth login --token <token>`),
	// enabling gated-repo downloads (meta-llama/*, google/gemma-*, mistralai/*). The
	// token is stored by `hf` itself (HF_HOME/~/.cache/huggingface), never by the
	// platform.
	Login(token string) error
	// Whoami returns the logged-in Hugging Face user (`hf auth whoami`, trimmed). It
	// returns an error when not logged in or `hf` is unavailable.
	Whoami() (string, error)
	// Logout clears the stored Hugging Face credentials (`hf auth logout`).
	Logout() error
}
