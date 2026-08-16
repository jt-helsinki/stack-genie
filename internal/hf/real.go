package hf

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// realClient is the production Client: it shells out to `hf` with HF_HOME pointed at
// the vLLM weight store so downloads land where `vllm serve` looks and cache
// list/remove operate on the same store.
//
// hardware bring-up: `hf download` fetches multi-GB weights over the network, so the
// real exec runs only on a provisioned host; unit tests inject Fake instead.
type realClient struct{}

// RealClient returns the production HF Client.
func RealClient() Client { return realClient{} }

// hfCommand builds an `hf` exec.Cmd with HF_HOME set to the vLLM store so every
// operation reads/writes the shared weight store.
func hfCommand(args ...string) (*exec.Cmd, error) {
	storeDir, err := StoreDir()
	if err != nil {
		return nil, err
	}
	command := exec.Command(BinaryPath(), args...)
	command.Env = append(os.Environ(), "HF_HOME="+storeDir)
	return command, nil
}

// Download runs `hf download <repo>` into the vLLM store, streaming stdout+stderr
// lines to progress (best-effort). A non-zero exit is surfaced as an error.
// Download runs `hf download <repo>` and streams the CLI's LIVE output (its tqdm
// progress bars) straight to progressOut so the caller can show download progress. The
// bars update in place with carriage returns (`\r`), NOT newlines, so we do NOT
// line-scan them (that would surface nothing until completion) — stdout+stderr are wired
// directly to progressOut verbatim, and when progressOut is a terminal/PTY hf renders its
// native progress bars in place. A nil progressOut discards the output. HF_HOME is set by
// hfCommand so weights land in the vLLM store.
func (realClient) Download(repo string, progressOut io.Writer) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return fmt.Errorf("hf: download requires a non-empty repo id")
	}
	command, err := hfCommand("download", repo)
	if err != nil {
		return err
	}
	if progressOut == nil {
		progressOut = io.Discard
	}
	command.Stdout = progressOut
	command.Stderr = progressOut
	if runErr := command.Run(); runErr != nil {
		return fmt.Errorf("hf: download %q: %w", repo, runErr)
	}
	return nil
}

// CacheList runs `hf cache ls -q` (repo ids only) and returns one CachedModel per
// non-empty line. `-q` prints just the repo ids, which is stable to parse; Size is
// left empty (the quiet form omits it). An `hf` that is missing or errors surfaces the
// error so the CLI can map it — EXCEPT a not-yet-populated store: on a fresh install
// (no model pulled) the HF cache dir does not exist and `hf cache ls` exits non-zero
// with "Cache directory not found", which is not a real error — an empty store is an
// empty list, so that case returns (nil, nil) rather than a spurious failure.
func (realClient) CacheList() ([]CachedModel, error) {
	command, err := hfCommand("cache", "ls", "-q")
	if err != nil {
		return nil, err
	}
	out, runErr := command.Output()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if isEmptyCacheStderr(string(exitErr.Stderr)) {
				return nil, nil // empty/not-yet-populated store — not an error
			}
			if trimmed := strings.TrimSpace(string(exitErr.Stderr)); trimmed != "" {
				return nil, fmt.Errorf("hf: cache ls: %w: %s", runErr, trimmed)
			}
		}
		return nil, fmt.Errorf("hf: cache ls: %w", runErr)
	}
	var models []CachedModel
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		repo := strings.TrimSpace(scanner.Text())
		if repo == "" {
			continue
		}
		// `hf cache ls -q` prints entries with a repo-TYPE prefix (e.g.
		// "model/mlx-community/Foo"). Strip it so Repo is the canonical bare id
		// ("mlx-community/Foo") that matches the curated list, the vLLM alias, and pull.
		models = append(models, CachedModel{Repo: stripCacheRepoType(repo)})
	}
	return models, nil
}

// cacheRepoTypes are the repo-TYPE prefixes `hf cache ls`/`rm` use on an entry id.
var cacheRepoTypes = []string{"model/", "dataset/", "space/"}

// stripCacheRepoType removes a leading repo-type prefix ("model/"/"dataset/"/"space/")
// from an `hf cache` entry id, yielding the bare "<org>/<name>" repo id.
func stripCacheRepoType(entry string) string {
	for _, prefix := range cacheRepoTypes {
		if trimmed, ok := strings.CutPrefix(entry, prefix); ok {
			return trimmed
		}
	}
	return entry
}

// cacheEntryID renders a bare repo id ("<org>/<name>") back into the `hf cache rm`
// entry-id form, which REQUIRES a repo-type prefix ("model/<org>/<name>"). A value that
// already carries a repo-type prefix is passed through unchanged.
func cacheEntryID(repo string) string {
	for _, prefix := range cacheRepoTypes {
		if strings.HasPrefix(repo, prefix) {
			return repo
		}
	}
	return "model/" + repo
}

// Login runs `hf auth login --token <token>` so subsequent downloads can reach gated
// repos. The token is passed only to `hf`, which owns its own credential store
// (HF_HOME/~/.cache/huggingface) — the platform never persists it. `--add-to-git-credential`
// is deliberately omitted.
func (realClient) Login(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("hf: login requires a non-empty token")
	}
	command, err := hfCommand("auth", "login", "--token", token)
	if err != nil {
		return err
	}
	if out, runErr := command.CombinedOutput(); runErr != nil {
		return fmt.Errorf("hf: auth login: %w: %s", runErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// Whoami runs `hf auth whoami` and returns the trimmed user id. A non-zero exit (e.g.
// "Not logged in") surfaces as an error.
func (realClient) Whoami() (string, error) {
	command, err := hfCommand("auth", "whoami")
	if err != nil {
		return "", err
	}
	out, runErr := command.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if runErr != nil {
		return "", fmt.Errorf("hf: auth whoami: %w: %s", runErr, trimmed)
	}
	return trimmed, nil
}

// Logout runs `hf auth logout` to clear the stored Hugging Face credentials.
func (realClient) Logout() error {
	command, err := hfCommand("auth", "logout")
	if err != nil {
		return err
	}
	if out, runErr := command.CombinedOutput(); runErr != nil {
		return fmt.Errorf("hf: auth logout: %w: %s", runErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// CacheRemove runs `hf cache rm <repo>` to delete a repo from the local cache.
// isEmptyCacheStderr reports whether `hf cache ls` failed only because the HF cache
// has not been created yet (a fresh install with no model pulled) — the store is simply
// empty, not broken. `hf` prints "Cache directory not found: <path>" (or "No cached
// repos") in that case; either is treated as an empty list, not an error.
func isEmptyCacheStderr(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "cache directory not found") || strings.Contains(lower, "no cached")
}

func (realClient) CacheRemove(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return fmt.Errorf("hf: cache rm requires a non-empty repo id")
	}
	// `hf cache rm` requires the repo-TYPE-prefixed entry id ("model/<org>/<name>"), not
	// the bare repo id — passing the bare form fails, which is why deletes silently did
	// nothing. Normalize to the entry-id form.
	entry := cacheEntryID(repo)
	command, err := hfCommand("cache", "rm", entry, "--yes")
	if err != nil {
		return err
	}
	if out, runErr := command.CombinedOutput(); runErr != nil {
		return fmt.Errorf("hf: cache rm %q: %w: %s", entry, runErr, strings.TrimSpace(string(out)))
	}
	return nil
}
