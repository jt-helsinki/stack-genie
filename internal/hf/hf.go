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
	"io"
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
// Hugging Face repo. Name is a short display handle — kept equal to Repo's basename so
// `ai models show <Name>` resolves the same as `ai models show <Repo>` before a pull
// (the ACTUAL gateway alias is always derived from Repo's basename at pull time via
// vllmDefaultAlias, never from Name, unless `--alias` overrides it). Repo is the full
// HF repo id passed to `hf download` / `vllm serve`, Description is a one-liner, and
// Size is an approximate on-disk weight size. ToolCallParser is the model's CONFIRMED
// vLLM `--tool-call-parser` value (per vLLM's tool-calling docs) — when set, `ai models
// pull` starts the server with `--enable-auto-tool-choice --tool-call-parser
// <value>` so agentic tool_choice="auto" requests work; when empty, no flags are
// added (tool_choice="auto" then fails with vLLM's own clear error — better than
// guessing a WRONG parser, which can corrupt tool calls silently instead of erroring).
// Only populate this from an authoritative source (vLLM's docs or the model vendor's
// own deployment guide) — never guess from family resemblance alone.
type CuratedModel struct {
	Name           string `json:"name"`
	Repo           string `json:"repo"`
	Description    string `json:"description,omitempty"`
	Size           string `json:"size,omitempty"`
	ToolCallParser string `json:"tool_call_parser,omitempty"`
}

// ToolCallParserFor returns the confirmed vLLM --tool-call-parser value for repo from
// the curated list for goos (empty when repo is not curated or has no confirmed
// parser — see CuratedModel.ToolCallParser).
func ToolCallParserFor(goos, repo string) string {
	for _, model := range CuratedModels(goos) {
		if model.Repo == repo {
			return model.ToolCallParser
		}
	}
	return ""
}

// The two curated lists below are HAND-CURATED — there is no auto-refresh and no live
// HF Hub search. curatedSafetensors (Linux/CUDA) carries the FULL user-specified roster
// (every model that resolves to a real HF repo); curatedMLX (darwin) is a SUBSET — only
// the roster models small enough to run on a Mac that ALSO ship an mlx-community/* build.
// The big frontier flagships are now ALSO carried on the MLX list wherever an
// mlx-community/* build actually exists — they run on high-RAM Apple Silicon (M-series
// Ultra/Max, 64–512 GB unified memory), so a low-bit MLX build IS surfaced on macOS even
// for the trillion-param MoEs; each such entry notes the RAM reality in its Description.
// The only roster models with NO mlx-community build stay Linux-only: Mistral Large 3 (no
// MLX repo at all), Kimi K3 (only a REAP-expert-pruned 2-bit build exists, not a
// faithful full-model quant), and Poolside Laguna S 2.1 (its MLX builds are third-party —
// poolside/Laguna-S-2.1-NVFP4-mlx, Vontra/Laguna-S-2.1-MLX-4bit — not under mlx-community/*).
// macOS surfaces only the MLX list, Linux only the
// safetensors list (see CuratedModels). To extend: add the GPU repo to
// curatedSafetensors and, only if the model is Mac-sized AND has an mlx-community/* build,
// its MLX repo to curatedMLX; verify each repo id resolves on https://huggingface.co (do
// NOT invent ids), note GATED repos in the Description ("gated — run `ai models login`"),
// and give an approximate on-disk size. Both lists are grouped/ordered by vendor.
//
// Curated from the live Hugging Face API as of 2026-08, to the user-specified roster:
// DeepSeek (V4, V3.2, R1); Meta (Muse Glimmer, Llama 4 Scout, Llama 4 Maverick); Google
// (Gemma 4, Gemma 3); MiniMax (M3, M2.7, M2.5); Mistral AI (Small 4, Large 3); MoonshotAI
// (Kimi K3, K2.6, K2.5); NVIDIA (Nemotron 3 Ultra, Super, Nano); Qwen (3.8, 3.6, 3.5);
// StepFun (Step-3.7-Flash, Step-3.5-Flash); Z-AI (GLM 5.2, 5.1, 5); JetBrains (Mellum).
// Every repo id below was verified to return HTTP 200 from GET /api/models/<repo>; the
// GATED repos (Llama 4 Scout/Maverick, Gemma 3) are marked in the Description.

// curatedMLX is the Apple-Silicon (darwin) curated set: mlx-community/* builds (4-bit
// preferred; a lower/mixed bit-width where that vendor ships no clean 4-bit) served on the
// Metal GPU via the vLLM-Metal plugin. It now spans the FULL roster wherever an
// mlx-community build exists — including the trillion-param flagships, which need high-RAM
// Apple Silicon (each big entry notes the RAM reality). Only Mistral Large 3 (no MLX repo)
// and Kimi K3 (only a REAP-pruned 2-bit build) stay Linux-only (see curatedSafetensors).
// Vendor order mirrors curatedSafetensors.
var curatedMLX = []CuratedModel{
	// DeepSeek
	{Name: "DeepSeek-V4-Pro-4bit", Repo: "mlx-community/DeepSeek-V4-Pro-4bit", Description: "DeepSeek V4 Pro, 4-bit — MoE flagship — extreme size, exceeds a single Mac's unified memory (>512 GB)", Size: "~837 GB"},
	{Name: "DeepSeek-V3.2-4bit", Repo: "mlx-community/DeepSeek-V3.2-4bit", Description: "DeepSeek V3.2, 4-bit — MoE — very large, needs 512 GB Apple Silicon", Size: "~378 GB"},
	{Name: "DeepSeek-R1-3bit", Repo: "mlx-community/DeepSeek-R1-3bit", Description: "DeepSeek R1, 3-bit — MoE reasoning (only full-R1 MLX quant) — very large, needs 512 GB Apple Silicon", Size: "~336 GB"},
	// Meta
	{Name: "Muse-Glimmer-30B-4bit", Repo: "mlx-community/Muse-Glimmer-30B-4bit", Description: "Meta Muse Glimmer 30B, 4-bit — general", Size: "~17 GB"},
	{Name: "Llama-4-Scout-17B-16E-Instruct-4bit", Repo: "mlx-community/Llama-4-Scout-17B-16E-Instruct-4bit", Description: "Meta Llama 4 Scout 17Bx16E, 4-bit — MoE multimodal (large)", Size: "~60 GB", ToolCallParser: "llama4_pythonic"},
	{Name: "Llama-4-Maverick-17B-128E-Instruct-4bit", Repo: "mlx-community/Llama-4-Maverick-17B-128E-Instruct-4bit", Description: "Meta Llama 4 Maverick 17Bx128E, 4-bit — MoE multimodal — very large, needs high-RAM Apple Silicon (256 GB+)", Size: "~226 GB", ToolCallParser: "llama4_pythonic"},
	// Google
	{Name: "gemma-4-31b-it-4bit", Repo: "mlx-community/gemma-4-31b-it-4bit", Description: "Google Gemma 4 31B instruct, 4-bit — multimodal flagship", Size: "~17 GB"},
	{Name: "gemma-4-26b-a4b-it-4bit", Repo: "mlx-community/gemma-4-26b-a4b-it-4bit", Description: "Google Gemma 4 26B-A4B instruct, 4-bit — MoE, low active-param — good coding fit on lower-RAM Macs", Size: "~14 GB"},
	{Name: "gemma-3-27b-it-4bit", Repo: "mlx-community/gemma-3-27b-it-4bit", Description: "Google Gemma 3 27B instruct, 4-bit — multimodal", Size: "~15 GB"},
	// Microsoft
	{Name: "phi-4-4bit", Repo: "mlx-community/phi-4-4bit", Description: "Microsoft Phi-4 14B, 4-bit — small, lightweight, fast on any Mac", Size: "~8 GB"},
	// MiniMax
	{Name: "MiniMax-M3-4bit", Repo: "mlx-community/MiniMax-M3-4bit", Description: "MiniMax M3, 4-bit — MoE flagship — very large, needs high-RAM Apple Silicon (256 GB+)", Size: "~241 GB"},
	{Name: "MiniMax-M2.7-4bit", Repo: "mlx-community/MiniMax-M2.7-4bit", Description: "MiniMax M2.7, 4-bit — MoE — large, needs high-RAM Apple Silicon (192 GB+)", Size: "~129 GB"},
	{Name: "MiniMax-M2.5-4bit", Repo: "mlx-community/MiniMax-M2.5-4bit", Description: "MiniMax M2.5, 4-bit — MoE — large, needs high-RAM Apple Silicon (192 GB+)", Size: "~129 GB"},
	// Mistral AI
	{Name: "Mistral-Small-4-119B-2603-4bit", Repo: "mlx-community/Mistral-Small-4-119B-2603-4bit", Description: "Mistral Small 4 119B, 4-bit — MoE — large, needs high-RAM Apple Silicon (192 GB+)", Size: "~136 GB"},
	{Name: "Devstral-Small-2505-4bit", Repo: "mlx-community/Devstral-Small-2505-4bit", Description: "Mistral Devstral Small 2505 24B, 4-bit — dense coding agent model, runs on 32 GB Macs", Size: "~13 GB"},
	// MoonshotAI
	{Name: "Kimi-K2.6-mxfp8", Repo: "mlx-community/Kimi-K2.6-mxfp8", Description: "Moonshot Kimi K2.6, mxfp8 8-bit (no clean 4-bit build) — extreme size, exceeds a single Mac's unified memory (>512 GB)", Size: "~1 TB"},
	{Name: "Kimi-K2.5-3bit", Repo: "mlx-community/Kimi-K2.5-3bit", Description: "Moonshot Kimi K2.5, 3-bit (smallest clean quant) — very large, needs 512 GB Apple Silicon", Size: "~449 GB"},
	// NVIDIA
	{Name: "Nemotron-3-Ultra-550B-A55B-4bit", Repo: "mlx-community/Nemotron-3-Ultra-550B-A55B-4bit", Description: "NVIDIA Nemotron 3 Ultra 550B-A55B, 4-bit — MoE — very large, needs 512 GB Apple Silicon", Size: "~347 GB"},
	{Name: "NVIDIA-Nemotron-3-Super-120B-A12B-4bit", Repo: "mlx-community/NVIDIA-Nemotron-3-Super-120B-A12B-4bit", Description: "NVIDIA Nemotron 3 Super 120B-A12B, 4-bit — MoE — needs high-RAM Apple Silicon (128 GB+)", Size: "~68 GB"},
	{Name: "NVIDIA-Nemotron-3-Nano-30B-A3B-4bit", Repo: "mlx-community/NVIDIA-Nemotron-3-Nano-30B-A3B-4bit", Description: "NVIDIA Nemotron 3 Nano 30B-A3B, 4-bit — MoE reasoning", Size: "~17 GB"},
	// Qwen
	// (Ornith-1.5-397B has no mlx-community build — see curatedSafetensors)
	{Name: "Qwen3.8-27B-4bit", Repo: "mlx-community/Qwen3.8-27B-4bit", Description: "Qwen3.8 27B, 4-bit — flagship dense", Size: "~15 GB", ToolCallParser: "hermes"},
	{Name: "Qwen3.6-27B-4bit", Repo: "mlx-community/Qwen3.6-27B-4bit", Description: "Qwen3.6 27B, 4-bit — dense general", Size: "~15 GB", ToolCallParser: "hermes"},
	{Name: "Qwen3.5-27B-4bit", Repo: "mlx-community/Qwen3.5-27B-4bit", Description: "Qwen3.5 27B, 4-bit — dense general", Size: "~15 GB", ToolCallParser: "hermes"},
	{Name: "Qwen3-Coder-Next-4bit", Repo: "mlx-community/Qwen3-Coder-Next-4bit", Description: "Qwen3-Coder-Next 80B-A3B, 4-bit — MoE coding flagship, low active-param — needs high-RAM Apple Silicon (64 GB+)", Size: "~43 GB", ToolCallParser: "qwen3_xml"},
	{Name: "Qwen3-Coder-480B-A35B-Instruct-4bit", Repo: "mlx-community/Qwen3-Coder-480B-A35B-Instruct-4bit", Description: "Qwen3-Coder 480B-A35B instruct, 4-bit — MoE coding flagship — very large, needs high-RAM Apple Silicon (256 GB+)", Size: "~270 GB", ToolCallParser: "qwen3_xml"},
	{Name: "Qwen3-Coder-30B-A3B-Instruct-4bit", Repo: "mlx-community/Qwen3-Coder-30B-A3B-Instruct-4bit", Description: "Qwen3-Coder 30B-A3B instruct, 4-bit — MoE coding, low active-param — sweet spot for local coding on 32 GB+ Macs", Size: "~17 GB", ToolCallParser: "qwen3_xml"},
	// StepFun
	{Name: "Step-3.7-Flash-4bit", Repo: "mlx-community/Step-3.7-Flash-4bit", Description: "StepFun Step-3.7-Flash, 4-bit — MoE — large, needs high-RAM Apple Silicon (128 GB+)", Size: "~111 GB"},
	{Name: "Step-3.5-Flash-4bit", Repo: "mlx-community/Step-3.5-Flash-4bit", Description: "StepFun Step-3.5-Flash, 4-bit — MoE — very large, needs high-RAM Apple Silicon (256 GB+)", Size: "~222 GB"},
	// Tencent
	{Name: "Hy3-preview-4bit", Repo: "mlx-community/Hy3-preview-4bit", Description: "Tencent Hunyuan Hy3 preview, 4-bit — MoE (295B/21B active) — large, needs high-RAM Apple Silicon (192 GB+)", Size: "~150 GB"},
	// Thinking Machines Lab
	{Name: "Inkling-mlx-4bit", Repo: "mlx-community/Inkling-mlx-4bit", Description: "Thinking Machines Inkling, 4-bit — MoE multimodal (975B/41B active) — very large, needs 512 GB Apple Silicon", Size: "~488 GB"},
	// Z-AI
	{Name: "GLM-5.2-4bit", Repo: "mlx-community/GLM-5.2-4bit", Description: "Z-AI GLM 5.2, 4-bit — MoE flagship — very large, needs 512 GB Apple Silicon", Size: "~418 GB"},
	{Name: "GLM-5.1-MXFP4-Q8", Repo: "mlx-community/GLM-5.1-MXFP4-Q8", Description: "Z-AI GLM 5.1, mxfp4/q8 mixed ~4-bit (no clean 4-bit build) — very large, needs 512 GB Apple Silicon", Size: "~406 GB"},
	{Name: "GLM-5-4bit", Repo: "mlx-community/GLM-5-4bit", Description: "Z-AI GLM 5, 4-bit — MoE — very large, needs 512 GB Apple Silicon", Size: "~419 GB"},
	// JetBrains
	{Name: "Mellum2-12B-A2.5B-Instruct-4bit", Repo: "mlx-community/Mellum2-12B-A2.5B-Instruct-4bit", Description: "JetBrains Mellum2 12B-A2.5B, 4-bit — MoE code model", Size: "~7 GB"},
}

// curatedSafetensors is the Linux/CUDA curated set: plain Hugging Face safetensors repos
// served on NVIDIA GPUs. It carries the FULL user-specified roster (curatedMLX is the
// Mac-sized subset). Sizes are the native bf16 weights. "gated — run `ai models login`"
// marks repos needing `hf` login / license acceptance.
var curatedSafetensors = []CuratedModel{
	// DeepSeek
	{Name: "DeepSeek-V4-Pro", Repo: "deepseek-ai/DeepSeek-V4-Pro", Description: "DeepSeek V4 Pro — frontier MoE flagship (very large)", Size: "~3.2 TB"},
	{Name: "DeepSeek-V3.2", Repo: "deepseek-ai/DeepSeek-V3.2", Description: "DeepSeek V3.2 — 685B MoE", Size: "~1.4 TB"},
	{Name: "DeepSeek-R1", Repo: "deepseek-ai/DeepSeek-R1", Description: "DeepSeek R1 — 671B MoE reasoning", Size: "~1.3 TB"},
	// Meta
	{Name: "Muse-Glimmer-30B", Repo: "meta-models/Muse-Glimmer-30B", Description: "Meta Muse Glimmer 30B — general", Size: "~60 GB"},
	{Name: "Llama-4-Scout-17B-16E-Instruct", Repo: "meta-llama/Llama-4-Scout-17B-16E-Instruct", Description: "Meta Llama 4 Scout 17Bx16E — MoE multimodal — gated, run `ai models login`", Size: "~217 GB", ToolCallParser: "llama4_pythonic"},
	{Name: "Llama-4-Maverick-17B-128E-Instruct", Repo: "meta-llama/Llama-4-Maverick-17B-128E-Instruct", Description: "Meta Llama 4 Maverick 17Bx128E — MoE multimodal — gated, run `ai models login`", Size: "~803 GB", ToolCallParser: "llama4_pythonic"},
	// Google
	{Name: "gemma-4-31B-it", Repo: "google/gemma-4-31B-it", Description: "Google Gemma 4 31B instruct — multimodal flagship", Size: "~63 GB"},
	{Name: "gemma-4-26b-a4b-it", Repo: "google/gemma-4-26b-a4b-it", Description: "Google Gemma 4 26B-A4B instruct — MoE, low active-param — good coding fit", Size: "~52 GB"},
	{Name: "gemma-3-27b-it", Repo: "google/gemma-3-27b-it", Description: "Google Gemma 3 27B instruct — multimodal — gated, run `ai models login`", Size: "~55 GB"},
	// Microsoft
	{Name: "phi-4", Repo: "microsoft/phi-4", Description: "Microsoft Phi-4 14B — small, lightweight, fast", Size: "~28 GB"},
	// MiniMax
	{Name: "MiniMax-M3", Repo: "MiniMaxAI/MiniMax-M3", Description: "MiniMax M3 — 427B MoE flagship (very large)", Size: "~854 GB"},
	{Name: "MiniMax-M2.7", Repo: "MiniMaxAI/MiniMax-M2.7", Description: "MiniMax M2.7 — 229B MoE", Size: "~457 GB"},
	{Name: "MiniMax-M2.5", Repo: "MiniMaxAI/MiniMax-M2.5", Description: "MiniMax M2.5 — 229B MoE", Size: "~457 GB"},
	// Mistral AI
	{Name: "Mistral-Small-4-119B-2603", Repo: "mistralai/Mistral-Small-4-119B-2603", Description: "Mistral Small 4 119B — MoE", Size: "~239 GB"},
	{Name: "Mistral-Large-3-675B-Instruct-2512", Repo: "mistralai/Mistral-Large-3-675B-Instruct-2512", Description: "Mistral Large 3 675B instruct — MoE flagship (very large)", Size: "~1.35 TB"},
	{Name: "Devstral-Small-2505", Repo: "mistralai/Devstral-Small-2505", Description: "Mistral Devstral Small 2505 24B — dense coding agent model", Size: "~48 GB"},
	// MoonshotAI
	{Name: "Kimi-K3", Repo: "moonshotai/Kimi-K3", Description: "Moonshot Kimi K3 — frontier MoE flagship (very large)", Size: "~5.6 TB"},
	{Name: "Kimi-K2.6", Repo: "moonshotai/Kimi-K2.6", Description: "Moonshot Kimi K2.6 — 1T MoE", Size: "~2.1 TB"},
	{Name: "Kimi-K2.5", Repo: "moonshotai/Kimi-K2.5", Description: "Moonshot Kimi K2.5 — 1T MoE", Size: "~2.1 TB"},
	// NVIDIA
	{Name: "NVIDIA-Nemotron-3-Ultra-550B-A55B-BF16", Repo: "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-BF16", Description: "NVIDIA Nemotron 3 Ultra 550B-A55B — MoE flagship (very large)", Size: "~1.1 TB"},
	{Name: "NVIDIA-Nemotron-3-Super-120B-A12B-BF16", Repo: "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-BF16", Description: "NVIDIA Nemotron 3 Super 120B-A12B — MoE reasoning", Size: "~247 GB"},
	{Name: "NVIDIA-Nemotron-3-Nano-30B-A3B-BF16", Repo: "nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B-BF16", Description: "NVIDIA Nemotron 3 Nano 30B-A3B — MoE reasoning", Size: "~63 GB"},
	// Ornith AI
	{Name: "Ornith-1.5-397B", Repo: "ornith-ai/Ornith-1.5-397B", Description: "Ornith 1.5 397B — MoE reasoning/coding — no mlx-community build, MLX-unavailable (very large)", Size: "~800 GB"},
	// Poolside
	{Name: "Laguna-S-2.1", Repo: "poolside/Laguna-S-2.1", Description: "Poolside Laguna S 2.1 — MoE long-horizon coding (FP8/BF16 native)", Size: "~270 GB"},
	// Qwen
	{Name: "Qwen3.8-2.4T-A95B", Repo: "Qwen/Qwen3.8-2.4T-A95B", Description: "Qwen3.8 Max, 2.4T-A95B — MoE flagship — no mlx-community build, MLX-unavailable (extreme size)", Size: "~4.89 TB"},
	{Name: "Qwen3.8-27B", Repo: "Qwen/Qwen3.8-27B", Description: "Qwen3.8 27B — flagship dense", Size: "~56 GB", ToolCallParser: "hermes"},
	{Name: "Qwen3.6-27B", Repo: "Qwen/Qwen3.6-27B", Description: "Qwen3.6 27B — dense general", Size: "~56 GB", ToolCallParser: "hermes"},
	{Name: "Qwen3.5-27B", Repo: "Qwen/Qwen3.5-27B", Description: "Qwen3.5 27B — dense general", Size: "~56 GB", ToolCallParser: "hermes"},
	{Name: "Qwen3-Coder-Next", Repo: "Qwen/Qwen3-Coder-Next", Description: "Qwen3-Coder-Next 80B-A3B — MoE coding flagship", Size: "~160 GB", ToolCallParser: "qwen3_xml"},
	{Name: "Qwen3-Coder-480B-A35B-Instruct", Repo: "Qwen/Qwen3-Coder-480B-A35B-Instruct", Description: "Qwen3-Coder 480B-A35B instruct — MoE coding flagship (very large)", Size: "~960 GB", ToolCallParser: "qwen3_xml"},
	{Name: "Qwen3-Coder-30B-A3B-Instruct", Repo: "Qwen/Qwen3-Coder-30B-A3B-Instruct", Description: "Qwen3-Coder 30B-A3B instruct — MoE coding, low active-param", Size: "~60 GB", ToolCallParser: "qwen3_xml"},
	// StepFun
	{Name: "Step-3.7-Flash", Repo: "stepfun-ai/Step-3.7-Flash", Description: "StepFun Step-3.7-Flash — 201B MoE", Size: "~403 GB"},
	{Name: "Step-3.5-Flash", Repo: "stepfun-ai/Step-3.5-Flash", Description: "StepFun Step-3.5-Flash — 199B MoE", Size: "~399 GB"},
	// Tencent
	{Name: "Hy3", Repo: "tencent/Hy3", Description: "Tencent Hunyuan Hy3 — 295B/21B active MoE", Size: "~598 GB"},
	// Thinking Machines Lab
	{Name: "Inkling", Repo: "thinkingmachines/Inkling", Description: "Thinking Machines Inkling — 975B/41B active MoE multimodal", Size: "~1.95 TB"},
	// Z-AI
	{Name: "GLM-5.2", Repo: "zai-org/GLM-5.2", Description: "Z-AI GLM 5.2 — 753B MoE flagship (very large)", Size: "~1.5 TB"},
	{Name: "GLM-5.1", Repo: "zai-org/GLM-5.1", Description: "Z-AI GLM 5.1 — 753B MoE (very large)", Size: "~1.5 TB"},
	{Name: "GLM-5", Repo: "zai-org/GLM-5", Description: "Z-AI GLM 5 — 753B MoE (very large)", Size: "~1.5 TB"},
	// JetBrains
	{Name: "Mellum2-12B-A2.5B-Instruct", Repo: "JetBrains/Mellum2-12B-A2.5B-Instruct", Description: "JetBrains Mellum2 12B-A2.5B — MoE code model", Size: "~24 GB"},
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
	Download(repo string, progressOut io.Writer) error
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
