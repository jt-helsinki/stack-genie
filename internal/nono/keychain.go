package nono

import (
	"bytes"
	"errors"
	"os/exec"
	"runtime"
	"time"

	"github.com/creack/pty"
)

// keychainPromptTimeout bounds how long storeCredentialDarwin waits for the
// `security` CLI's two interactive password prompts before giving up.
const keychainPromptTimeout = 5 * time.Second

// StoreCredential stores value in the host OS keystore under nono's own
// convention (service "nono", username/account name), matching exactly what
// nono's own documentation recommends for feeding it `--env-credential-map`/
// `env_credentials` (see the OpenCode client integration guide at
// https://nono.sh/docs/cli/clients/opencode): macOS Keychain via `security`,
// Linux via `secret-tool` (Secret Service). This is how the platform's scoped
// LiteLLM virtual key reaches an agent CLI's process environment without ever
// being written to a profile, config file, or plain host env file — only the
// credential NAME travels through argv/config; the secret lives solely in the
// OS keystore. Overwrites any existing entry under the same name.
func StoreCredential(name, value string) error {
	switch runtime.GOOS {
	case "darwin":
		// -U updates in place if the entry already exists, matching nono's own
		// documented re-run-safe usage.
		return storeCredentialDarwin(name, value)
	case "linux":
		command := exec.Command("secret-tool", "store", "--label=nono: "+name, "service", "nono", "username", name)
		command.Stdin = bytes.NewBufferString(value)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			return errors.New(output.String())
		}
		return nil
	default:
		return errors.New("nono: credential storage is not implemented on " + runtime.GOOS)
	}
}

// DeleteCredential removes a credential previously stored by StoreCredential.
// Best-effort: a missing entry is not an error (matches os.Remove-on-absent
// callers elsewhere in this codebase that treat "already gone" as success).
func DeleteCredential(name string) error {
	switch runtime.GOOS {
	case "darwin":
		_ = run("security", "delete-generic-password", "-s", "nono", "-a", name)
		return nil
	case "linux":
		_ = run("secret-tool", "clear", "service", "nono", "username", name)
		return nil
	default:
		return nil
	}
}

// storeCredentialDarwin runs `security add-generic-password -w` with the value
// omitted from argv (a bare -w makes `security` prompt interactively instead),
// feeding the value over a pty when the CLI's two "...password..." prompts
// appear. A bare os/exec.Cmd.Stdin pipe does NOT work here: `security` reads
// its prompts via getpass(3)/readpassphrase(3) directly against the
// controlling terminal, not the process's stdin fd — verified live, a pty is
// required. This keeps the secret out of argv (readable via `ps`/
// /proc-equivalent listings) entirely.
func storeCredentialDarwin(name, value string) error {
	command := exec.Command("security", "add-generic-password", "-U", "-s", "nono", "-a", name, "-w")
	ptmx, err := pty.Start(command)
	if err != nil {
		return err
	}
	defer func() { _ = ptmx.Close() }()

	timer := time.AfterFunc(keychainPromptTimeout, func() { _ = command.Process.Kill() })
	defer timer.Stop()

	done := make(chan error, 1)
	go func() {
		// Keep draining the pty until it closes (EOF on the child's exit), even
		// after both prompts are answered — the child's remaining writes (e.g. a
		// trailing newline/status) would otherwise block on a full pty buffer
		// with nothing reading it, which hangs the child before it can exit.
		buf := make([]byte, 256)
		written := 0
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 && written < 2 && bytes.Contains(buf[:n], []byte("assword")) {
				if _, writeErr := ptmx.Write([]byte(value + "\n")); writeErr != nil {
					done <- writeErr
					return
				}
				written++
			}
			if readErr != nil {
				break
			}
		}
		if written < 2 {
			done <- errors.New("security add-generic-password: did not see the expected password prompts")
			return
		}
		done <- nil
	}()

	waitErr := command.Wait()
	if promptErr := <-done; promptErr != nil {
		return promptErr
	}
	if waitErr != nil {
		return errors.New("security add-generic-password: " + waitErr.Error())
	}
	return nil
}

func run(name string, args ...string) error {
	command := exec.Command(name, args...)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return errors.New(output.String())
	}
	return nil
}
