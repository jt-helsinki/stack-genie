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
// repo is GATED (needs `hf` login / license click-through — meta-llama/*, google/gemma*),
// and give an approximate on-disk size. All repo ids below were verified against the HF
// API. The mlx-community/* mirrors are ungated even where the upstream GPU repo is gated.

// curatedMLX is the Apple-Silicon (darwin) curated set: mlx-community/* 4-bit repos
// served on the Metal GPU via the vLLM-Metal plugin.
var curatedMLX = []CuratedModel{
	// Qwen2.5 (general)
	{Name: "Qwen2.5-7B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit", Description: "Qwen2.5 7B instruct, 4-bit — strong general/coding model", Size: "~4.3 GB"},
	{Name: "Qwen2.5-14B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-14B-Instruct-4bit", Description: "Qwen2.5 14B instruct, 4-bit — larger general model", Size: "~8.1 GB"},
	{Name: "Qwen2.5-32B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-32B-Instruct-4bit", Description: "Qwen2.5 32B instruct, 4-bit", Size: "~18 GB"},
	{Name: "Qwen2.5-72B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-72B-Instruct-4bit", Description: "Qwen2.5 72B instruct, 4-bit — flagship", Size: "~41 GB"},
	// Qwen2.5-Coder (code-specialised)
	{Name: "Qwen2.5-Coder-7B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-Coder-7B-Instruct-4bit", Description: "Qwen2.5-Coder 7B, 4-bit — code-specialised", Size: "~4.3 GB"},
	{Name: "Qwen2.5-Coder-14B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-Coder-14B-Instruct-4bit", Description: "Qwen2.5-Coder 14B, 4-bit — code-specialised", Size: "~8.1 GB"},
	{Name: "Qwen2.5-Coder-32B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-Coder-32B-Instruct-4bit", Description: "Qwen2.5-Coder 32B, 4-bit — top open coding model", Size: "~18 GB"},
	// Qwen3
	{Name: "Qwen3-8B-4bit", Repo: "mlx-community/Qwen3-8B-4bit", Description: "Qwen3 8B, 4-bit — hybrid reasoning", Size: "~4.6 GB"},
	{Name: "Qwen3-32B-4bit", Repo: "mlx-community/Qwen3-32B-4bit", Description: "Qwen3 32B, 4-bit — hybrid reasoning", Size: "~18 GB"},
	{Name: "Qwen3-30B-A3B-4bit", Repo: "mlx-community/Qwen3-30B-A3B-4bit", Description: "Qwen3 30B-A3B, 4-bit — MoE (3B active)", Size: "~17 GB"},
	// Llama
	{Name: "Llama-3.2-3B-Instruct-4bit", Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit", Description: "Llama 3.2 3B instruct, 4-bit — small & fast", Size: "~1.8 GB"},
	{Name: "Meta-Llama-3.1-8B-Instruct-4bit", Repo: "mlx-community/Meta-Llama-3.1-8B-Instruct-4bit", Description: "Llama 3.1 8B instruct, 4-bit", Size: "~4.5 GB"},
	{Name: "Llama-3.3-70B-Instruct-4bit", Repo: "mlx-community/Llama-3.3-70B-Instruct-4bit", Description: "Llama 3.3 70B instruct, 4-bit — flagship", Size: "~40 GB"},
	// Mistral / Mixtral
	{Name: "Mistral-7B-Instruct-v0.3-4bit", Repo: "mlx-community/Mistral-7B-Instruct-v0.3-4bit", Description: "Mistral 7B instruct v0.3, 4-bit", Size: "~4.1 GB"},
	{Name: "Mistral-Nemo-Instruct-2407-4bit", Repo: "mlx-community/Mistral-Nemo-Instruct-2407-4bit", Description: "Mistral Nemo 12B instruct, 4-bit — 128k context", Size: "~6.9 GB"},
	{Name: "Mistral-Small-24B-Instruct-2501-4bit", Repo: "mlx-community/Mistral-Small-24B-Instruct-2501-4bit", Description: "Mistral Small 24B instruct, 4-bit", Size: "~13 GB"},
	{Name: "Mixtral-8x7B-Instruct-v0.1-4bit", Repo: "mlx-community/Mixtral-8x7B-Instruct-v0.1-4bit", Description: "Mixtral 8x7B instruct, 4-bit — MoE (47B total)", Size: "~26 GB"},
	// Google Gemma
	{Name: "gemma-2-9b-it-4bit", Repo: "mlx-community/gemma-2-9b-it-4bit", Description: "Google Gemma 2 9B instruct, 4-bit", Size: "~5.2 GB"},
	{Name: "gemma-2-27b-it-4bit", Repo: "mlx-community/gemma-2-27b-it-4bit", Description: "Google Gemma 2 27B instruct, 4-bit", Size: "~15 GB"},
	{Name: "gemma-3-27b-it-4bit", Repo: "mlx-community/gemma-3-27b-it-4bit", Description: "Google Gemma 3 27B instruct, 4-bit — multimodal", Size: "~17 GB"},
	// Microsoft Phi
	{Name: "Phi-3.5-mini-instruct-4bit", Repo: "mlx-community/Phi-3.5-mini-instruct-4bit", Description: "Microsoft Phi-3.5 mini instruct, 4-bit — compact", Size: "~2.2 GB"},
	{Name: "phi-4-4bit", Repo: "mlx-community/phi-4-4bit", Description: "Microsoft Phi-4 14B, 4-bit — strong reasoning", Size: "~8.2 GB"},
	// DeepSeek R1 distills
	{Name: "DeepSeek-R1-Distill-Qwen-7B-4bit", Repo: "mlx-community/DeepSeek-R1-Distill-Qwen-7B-4bit", Description: "DeepSeek-R1 distill (Qwen 7B), 4-bit — reasoning", Size: "~4.3 GB"},
	{Name: "DeepSeek-R1-Distill-Qwen-32B-4bit", Repo: "mlx-community/DeepSeek-R1-Distill-Qwen-32B-4bit", Description: "DeepSeek-R1 distill (Qwen 32B), 4-bit — reasoning", Size: "~18 GB"},
}

// curatedSafetensors is the Linux/CUDA curated set: plain Hugging Face safetensors repos
// served on NVIDIA GPUs. Model-for-model aligned with curatedMLX (same index = same
// model). Sizes are the bf16/fp16 weights. "(gated repo)" marks repos needing `hf` login.
var curatedSafetensors = []CuratedModel{
	// Qwen2.5 (general)
	{Name: "Qwen2.5-7B-Instruct", Repo: "Qwen/Qwen2.5-7B-Instruct", Description: "Qwen2.5 7B instruct — strong general/coding model", Size: "~15 GB"},
	{Name: "Qwen2.5-14B-Instruct", Repo: "Qwen/Qwen2.5-14B-Instruct", Description: "Qwen2.5 14B instruct — larger general model", Size: "~30 GB"},
	{Name: "Qwen2.5-32B-Instruct", Repo: "Qwen/Qwen2.5-32B-Instruct", Description: "Qwen2.5 32B instruct", Size: "~66 GB"},
	{Name: "Qwen2.5-72B-Instruct", Repo: "Qwen/Qwen2.5-72B-Instruct", Description: "Qwen2.5 72B instruct — flagship", Size: "~145 GB"},
	// Qwen2.5-Coder (code-specialised)
	{Name: "Qwen2.5-Coder-7B-Instruct", Repo: "Qwen/Qwen2.5-Coder-7B-Instruct", Description: "Qwen2.5-Coder 7B — code-specialised", Size: "~15 GB"},
	{Name: "Qwen2.5-Coder-14B-Instruct", Repo: "Qwen/Qwen2.5-Coder-14B-Instruct", Description: "Qwen2.5-Coder 14B — code-specialised", Size: "~30 GB"},
	{Name: "Qwen2.5-Coder-32B-Instruct", Repo: "Qwen/Qwen2.5-Coder-32B-Instruct", Description: "Qwen2.5-Coder 32B — top open coding model", Size: "~66 GB"},
	// Qwen3
	{Name: "Qwen3-8B", Repo: "Qwen/Qwen3-8B", Description: "Qwen3 8B — hybrid reasoning", Size: "~16 GB"},
	{Name: "Qwen3-32B", Repo: "Qwen/Qwen3-32B", Description: "Qwen3 32B — hybrid reasoning", Size: "~66 GB"},
	{Name: "Qwen3-30B-A3B", Repo: "Qwen/Qwen3-30B-A3B", Description: "Qwen3 30B-A3B — MoE (3B active)", Size: "~61 GB"},
	// Llama
	{Name: "Llama-3.2-3B-Instruct", Repo: "meta-llama/Llama-3.2-3B-Instruct", Description: "Llama 3.2 3B instruct — small & fast (gated repo)", Size: "~6.5 GB"},
	{Name: "Llama-3.1-8B-Instruct", Repo: "meta-llama/Llama-3.1-8B-Instruct", Description: "Llama 3.1 8B instruct (gated repo)", Size: "~16 GB"},
	{Name: "Llama-3.3-70B-Instruct", Repo: "meta-llama/Llama-3.3-70B-Instruct", Description: "Llama 3.3 70B instruct — flagship (gated repo)", Size: "~141 GB"},
	// Mistral / Mixtral
	{Name: "Mistral-7B-Instruct-v0.3", Repo: "mistralai/Mistral-7B-Instruct-v0.3", Description: "Mistral 7B instruct v0.3", Size: "~15 GB"},
	{Name: "Mistral-Nemo-Instruct-2407", Repo: "mistralai/Mistral-Nemo-Instruct-2407", Description: "Mistral Nemo 12B instruct — 128k context", Size: "~25 GB"},
	{Name: "Mistral-Small-24B-Instruct-2501", Repo: "mistralai/Mistral-Small-24B-Instruct-2501", Description: "Mistral Small 24B instruct", Size: "~47 GB"},
	{Name: "Mixtral-8x7B-Instruct-v0.1", Repo: "mistralai/Mixtral-8x7B-Instruct-v0.1", Description: "Mixtral 8x7B instruct — MoE (47B total)", Size: "~93 GB"},
	// Google Gemma
	{Name: "gemma-2-9b-it", Repo: "google/gemma-2-9b-it", Description: "Google Gemma 2 9B instruct (gated repo)", Size: "~18 GB"},
	{Name: "gemma-2-27b-it", Repo: "google/gemma-2-27b-it", Description: "Google Gemma 2 27B instruct (gated repo)", Size: "~54 GB"},
	{Name: "gemma-3-27b-it", Repo: "google/gemma-3-27b-it", Description: "Google Gemma 3 27B instruct — multimodal (gated repo)", Size: "~54 GB"},
	// Microsoft Phi
	{Name: "Phi-3.5-mini-instruct", Repo: "microsoft/Phi-3.5-mini-instruct", Description: "Microsoft Phi-3.5 mini instruct — compact", Size: "~7.7 GB"},
	{Name: "phi-4", Repo: "microsoft/phi-4", Description: "Microsoft Phi-4 14B — strong reasoning", Size: "~29 GB"},
	// DeepSeek R1 distills
	{Name: "DeepSeek-R1-Distill-Qwen-7B", Repo: "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", Description: "DeepSeek-R1 distill (Qwen 7B) — reasoning", Size: "~15 GB"},
	{Name: "DeepSeek-R1-Distill-Qwen-32B", Repo: "deepseek-ai/DeepSeek-R1-Distill-Qwen-32B", Description: "DeepSeek-R1 distill (Qwen 32B) — reasoning", Size: "~66 GB"},
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
