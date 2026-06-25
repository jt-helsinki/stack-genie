package uihosts

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/hostsfile"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// DefaultHostsPath is the production hosts file location.
const DefaultHostsPath = hostsfile.DefaultPath

// HostsAction is the decision SyncHosts / RemoveHosts reached, for reporting/tests.
type HostsAction string

const (
	// HostsUpToDate: the managed block already matches (or nothing to remove) —
	// nothing was done.
	HostsUpToDate HostsAction = "up-to-date"
	// HostsWritten: the platform wrote the file (with consent, via the writer).
	HostsWritten HostsAction = "written"
	// HostsManual: the platform did NOT write (no TTY/--json, declined, or a write
	// failure) and printed the manual block instead — the caller still succeeds.
	HostsManual HostsAction = "manual"
	// HostsSkipped: not applicable (no UI subdomains to manage).
	HostsSkipped HostsAction = "skipped"
)

// HostsSync configures SyncHosts / RemoveHosts. Everything external is injected so
// the decision + the rendered block + the manual-instruction message are
// unit-testable without touching the real /etc/hosts or shelling out to sudo.
type HostsSync struct {
	// Path is the hosts file to manage (DefaultHostsPath in production; a temp
	// file in tests).
	Path string
	// Domain is the resolved platform base domain (runtime Info.ResolveDomain()).
	Domain string
	// Interactive is true on a TTY (not --json) — only then do we ask for consent
	// and attempt the privileged write; otherwise we print the manual block.
	Interactive bool
	// Out receives the manual-instruction message (the HostsManual path).
	Out io.Writer
	// Consent asks the user to approve writing /etc/hosts (needs sudo). Called only
	// when Interactive and an update is needed; nil is treated as "no".
	Consent func(prompt string) (bool, error)
	// Write performs the privileged write of the planned bytes to Path (the
	// `sudo cp` seam). nil falls back to the default sudo writer. Injected as a
	// fake in tests.
	Write func(path string, content []byte) error
}

// ManageHostsForRole reports whether the deployment role manages /etc/hosts: only
// STANDALONE does (server needs real DNS, a client runs no local service tier).
// The empty role is treated as standalone.
func ManageHostsForRole(role string) bool {
	return role == runtime.RoleStandalone || role == ""
}

// SyncHosts points the platform's UI subdomains (litellm.<domain>)
// at 127.0.0.1 in /etc/hosts, for STANDALONE mode. It:
//
//   - Plans the new bytes (hostsfile.Plan) — if the managed block is already up to
//     date, it does nothing (HostsUpToDate).
//   - Otherwise, on a TTY, prompts for consent (the write needs sudo); on consent
//     it writes the planned bytes via the injected writer (HostsWritten).
//   - On no-TTY/--json, a declined prompt, or a write failure, it does NOT fail the
//     caller — it prints the exact block to add manually (HostsManual).
//
// It returns the action taken (for the report/tests) and never an error that
// should fail setup: a write problem degrades to the manual message.
func SyncHosts(sync HostsSync) (HostsAction, error) {
	entries := Entries(sync.Domain)
	if len(entries) == 0 {
		return HostsSkipped, nil
	}

	planned, changed, err := hostsfile.Plan(sync.Path, entries)
	if err != nil {
		writeManual(sync)
		return HostsManual, nil
	}
	if !changed {
		return HostsUpToDate, nil
	}

	// Non-interactive: print the manual block (no prompt, no sudo).
	if !sync.Interactive || sync.Consent == nil {
		writeManual(sync)
		return HostsManual, nil
	}
	approved, consentErr := sync.Consent(
		"Update /etc/hosts so the platform UIs resolve by name (needs sudo)?")
	if consentErr != nil || !approved {
		writeManual(sync)
		return HostsManual, nil
	}

	if sync.Out != nil {
		_, _ = fmt.Fprintln(sync.Out, "Updating /etc/hosts (sudo) — enter your system login password if prompted…")
	}
	if writeErr := writerOf(sync)(sync.Path, planned); writeErr != nil {
		if sync.Out != nil {
			_, _ = fmt.Fprintf(sync.Out, "could not update %s (%s) — add it yourself:\n", sync.Path, writeErr)
		}
		writeManual(sync)
		return HostsManual, nil
	}
	return HostsWritten, nil
}

// RemoveHosts strips the platform's managed /etc/hosts block (the inverse of
// SyncHosts), for the standalone teardown path. Like the sync, it never fails the
// caller: a write problem just leaves the block in place and returns HostsManual
// with a hint. It mirrors HostsSync's injection points so uninstall's decision is
// unit-testable. The consent/Interactive fields are ignored — teardown removes the
// block best-effort (the user already chose to uninstall).
func RemoveHosts(sync HostsSync) (HostsAction, error) {
	planned, changed, err := hostsfile.Plan(sync.Path, nil) // nil entries → remove the block
	if err != nil || !changed {
		return HostsUpToDate, nil // nothing present (or unreadable) — nothing to remove
	}
	if writeErr := writerOf(sync)(sync.Path, planned); writeErr != nil {
		if sync.Out != nil {
			_, _ = fmt.Fprintf(sync.Out, "could not remove the platform block from %s (%s) — remove it manually\n", sync.Path, writeErr)
		}
		return HostsManual, nil
	}
	return HostsWritten, nil
}

// HostsStatus reports whether the platform's managed UI-subdomain block is
// present in path and, if so, whether it matches the entries for domain. It is
// read-only (hostsfile.Status) — the doctor resolution-status check.
func HostsStatus(path, domain string) (present, upToDate bool, err error) {
	return hostsfile.Status(path, Entries(domain))
}

// writerOf returns the configured Write, or the default sudo writer.
func writerOf(sync HostsSync) func(string, []byte) error {
	if sync.Write != nil {
		return sync.Write
	}
	return sudoWriteHosts
}

// writeManual prints the manual /etc/hosts instructions (the exact block to add)
// to the configured Out, if any.
func writeManual(sync HostsSync) {
	if sync.Out == nil {
		return
	}
	_, _ = io.WriteString(sync.Out, ManualMessage(sync.Domain))
}

// sudoWriteHosts is the production privileged write: it pipes the planned bytes to
// `sudo tee <path>`. This is the ONE seam that shells out (it needs root for
// /etc/hosts) — it is replaced by a fake in tests.
//
// hardware bring-up: the live `sudo cp` over /etc/hosts is exercised on a
// provisioned host (it prompts for the sudo password on the TTY).
func sudoWriteHosts(path string, content []byte) error {
	// Write via `sudo tee` with the content on stdin — the canonical "write a file
	// as root" idiom. It needs no root-readable temp path (an earlier `cp` from the
	// per-user $TMPDIR could fail), and stderr is captured so a real failure isn't
	// reduced to a bare "exit status 1". tee echoes stdin to stdout, which we drop;
	// sudo prompts for the password on the controlling terminal (/dev/tty), so the
	// content on stdin does not interfere.
	// #nosec G204 — fixed argv; path is the platform's own hosts target.
	cmd := exec.Command("sudo", "-p", "[ai] enter your login password to update "+path+": ", "tee", path)
	cmd.Stdin = bytes.NewReader(content)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	return nil
}
