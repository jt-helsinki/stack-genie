package hf

import (
	"bufio"
	"fmt"
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
func (realClient) Download(repo string, progress func(line string)) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return fmt.Errorf("hf: download requires a non-empty repo id")
	}
	command, err := hfCommand("download", repo)
	if err != nil {
		return err
	}
	pipe, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = command.Stdout
	if startErr := command.Start(); startErr != nil {
		return fmt.Errorf("hf: start download %q: %w", repo, startErr)
	}
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if progress != nil {
			progress(scanner.Text())
		}
	}
	if waitErr := command.Wait(); waitErr != nil {
		return fmt.Errorf("hf: download %q: %w", repo, waitErr)
	}
	return nil
}

// CacheList runs `hf cache ls -q` (repo ids only) and returns one CachedModel per
// non-empty line. `-q` prints just the repo ids, which is stable to parse; Size is
// left empty (the quiet form omits it). An `hf` that is missing or errors surfaces the
// error so the CLI can map it.
func (realClient) CacheList() ([]CachedModel, error) {
	command, err := hfCommand("cache", "ls", "-q")
	if err != nil {
		return nil, err
	}
	out, runErr := command.Output()
	if runErr != nil {
		return nil, fmt.Errorf("hf: cache ls: %w", runErr)
	}
	var models []CachedModel
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		repo := strings.TrimSpace(scanner.Text())
		if repo == "" {
			continue
		}
		models = append(models, CachedModel{Repo: repo})
	}
	return models, nil
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
func (realClient) CacheRemove(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return fmt.Errorf("hf: cache rm requires a non-empty repo id")
	}
	command, err := hfCommand("cache", "rm", repo)
	if err != nil {
		return err
	}
	if out, runErr := command.CombinedOutput(); runErr != nil {
		return fmt.Errorf("hf: cache rm %q: %w: %s", repo, runErr, strings.TrimSpace(string(out)))
	}
	return nil
}
