package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/multica"
	"github.com/jt-helsinki/stack-genie/internal/nono"
)

// multicaTaskFixture builds a HOME with a shim dir and a real-binary dir, both holding
// an executable named `claude`, PATH ordered shim-first (as the daemon's is), and a
// task workdir.
func multicaTaskFixture(test *testing.T) (realBinary, workdir string) {
	test.Helper()
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("MULTICA_WORKSPACES_ROOT", filepath.Join(home, "mws"))
	shimDir, err := multica.ShimDir()
	if err != nil {
		test.Fatal(err)
	}
	realDir := filepath.Join(home, "realbin")
	for _, dir := range []string{shimDir, realDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			test.Fatal(err)
		}
	}
	test.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)
	workdir = filepath.Join(home, "mws", "task1")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		test.Fatal(err)
	}
	return filepath.Join(realDir, "claude"), workdir
}

// The sandboxed launch must run the REAL binary (never its own shim — that would
// loop), in the task dir, with the shared key injected by keystore NAME only.
func TestRunMulticaTaskLaunchesRealBinaryInNono(test *testing.T) {
	realBinary, workdir := multicaTaskFixture(test)
	runner := &nono.FakeRunner{}
	manager := testManager(runner, &fakeKeyMinter{}, newFakeCredentialStore())

	if err := manager.RunMulticaTask("claude-code", workdir, []string{"-p", "hi"}); err != nil {
		test.Fatal(err)
	}
	if len(runner.RunCalls) != 1 {
		test.Fatalf("RunCalls = %d, want 1", len(runner.RunCalls))
	}
	call := runner.RunCalls[0]
	if call.Command[0] != realBinary || call.Command[1] != "-p" || call.Command[2] != "hi" {
		test.Errorf("command = %v, want the real binary %s with args", call.Command, realBinary)
	}
	if call.Dir != workdir || !call.AllowCWD {
		test.Errorf("dir/allow-cwd = %q/%v, want %q/true", call.Dir, call.AllowCWD, workdir)
	}
	if got := call.EnvCredentialMap["aip-gateway-multica"]; got != agentcfg.ClaudeAuthEnvVar {
		test.Errorf("credential map = %v, want aip-gateway-multica → %s", call.EnvCredentialMap, agentcfg.ClaudeAuthEnvVar)
	}
	// The profile lives outside the worktree and grants the workspaces root.
	if strings.HasPrefix(call.Profile, workdir) {
		test.Errorf("profile %s must not be written into the task worktree", call.Profile)
	}
	raw, err := os.ReadFile(call.Profile)
	if err != nil {
		test.Fatal(err)
	}
	var profile nono.Profile
	if err := json.Unmarshal(raw, &profile); err != nil {
		test.Fatal(err)
	}
	found := false
	for _, allowed := range profile.Filesystem.Allow {
		found = found || allowed == filepath.Join(os.Getenv("HOME"), "mws")
	}
	if !found {
		test.Errorf("profile filesystem allow = %v, want the workspaces root", profile.Filesystem.Allow)
	}
}

// The daemon's CLI-detection probe must neither sandbox nor mint anything.
func TestRunMulticaTaskVersionProbeBypassesSandbox(test *testing.T) {
	_, workdir := multicaTaskFixture(test)
	runner := &nono.FakeRunner{}
	keys := &fakeKeyMinter{}
	manager := testManager(runner, keys, newFakeCredentialStore())

	if err := manager.RunMulticaTask("claude-code", workdir, []string{"--version"}); err != nil {
		test.Fatal(err)
	}
	if len(runner.RunCalls) != 0 || len(keys.generated) != 0 {
		test.Errorf("version probe sandboxed or minted a key: runs=%d keys=%d", len(runner.RunCalls), len(keys.generated))
	}
}

// One shared key, minted once at daemon start — a per-task re-mint would revoke every
// concurrently running task's key.
func TestEnsureMulticaKeyMintsSharedKey(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	keys, creds := &fakeKeyMinter{}, newFakeCredentialStore()
	manager := testManager(&nono.FakeRunner{}, keys, creds)
	if err := manager.EnsureMulticaKey(); err != nil {
		test.Fatal(err)
	}
	if len(keys.generated) != 1 || keys.generated[0].Alias != "multica" {
		test.Errorf("generated = %v, want one key aliased multica", keys.generated)
	}
	if creds.stored["aip-gateway-multica"] != "sk-fake-key" {
		test.Errorf("stored = %v, want aip-gateway-multica in the keystore", creds.stored)
	}
}

func TestRunMulticaTaskUnknownCLI(test *testing.T) {
	multicaTaskFixture(test)
	manager := testManager(&nono.FakeRunner{}, &fakeKeyMinter{}, newFakeCredentialStore())
	if err := manager.RunMulticaTask("bogus", test.TempDir(), nil); err == nil {
		test.Fatal("expected an unknown-CLI error")
	}
}
