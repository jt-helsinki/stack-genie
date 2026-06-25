package uihosts

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/hostsfile"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

func tempHosts(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	return path
}

func TestManageHostsForRole(t *testing.T) {
	for _, role := range []string{runtime.RoleStandalone, ""} {
		if !ManageHostsForRole(role) {
			t.Errorf("role %q should manage /etc/hosts", role)
		}
	}
	for _, role := range []string{runtime.RoleServer, runtime.RoleClient} {
		if ManageHostsForRole(role) {
			t.Errorf("role %q must NOT manage /etc/hosts", role)
		}
	}
}

func TestSyncHostsUpToDateDoesNothing(t *testing.T) {
	path := tempHosts(t, "")
	if _, err := hostsfile.Apply(path, Entries("aip.local")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	wrote := false
	action, err := SyncHosts(HostsSync{
		Path: path, Domain: "aip.local", Interactive: true,
		Consent: func(string) (bool, error) { return true, nil },
		Write:   func(string, []byte) error { wrote = true; return nil },
	})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}
	if action != HostsUpToDate {
		t.Fatalf("expected up-to-date, got %q", action)
	}
	if wrote {
		t.Fatal("must not write when already up to date")
	}
}

func TestSyncHostsConsentWrites(t *testing.T) {
	path := tempHosts(t, "127.0.0.1\tlocalhost\n")
	var gotPath string
	var gotContent []byte
	action, err := SyncHosts(HostsSync{
		Path: path, Domain: "aip.local", Interactive: true,
		Consent: func(string) (bool, error) { return true, nil },
		Write: func(writePath string, content []byte) error {
			gotPath = writePath
			gotContent = content
			return nil
		},
	})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}
	if action != HostsWritten {
		t.Fatalf("expected written, got %q", action)
	}
	if gotPath != path {
		t.Fatalf("write path %q != %q", gotPath, path)
	}
	if !bytes.Contains(gotContent, []byte("litellm.aip.local")) || bytes.Contains(gotContent, []byte("chat.aip.local")) {
		t.Fatalf("planned bytes should contain litellm.aip.local and not chat.aip.local:\n%s", gotContent)
	}
	if !bytes.Contains(gotContent, []byte("127.0.0.1\tlocalhost")) {
		t.Fatalf("planned bytes must preserve existing entries:\n%s", gotContent)
	}
}

func TestSyncHostsNonInteractivePrintsManual(t *testing.T) {
	path := tempHosts(t, "")
	var out bytes.Buffer
	wrote := false
	action, _ := SyncHosts(HostsSync{
		Path: path, Domain: "aip.local", Interactive: false, Out: &out,
		Write: func(string, []byte) error { wrote = true; return nil },
	})
	if action != HostsManual {
		t.Fatalf("expected manual, got %q", action)
	}
	if wrote {
		t.Fatal("non-interactive must not write")
	}
	if !strings.Contains(out.String(), "/etc/hosts") || !strings.Contains(out.String(), "litellm.aip.local") {
		t.Fatalf("manual message missing:\n%s", out.String())
	}
}

func TestSyncHostsDeclinedPrintsManual(t *testing.T) {
	path := tempHosts(t, "")
	var out bytes.Buffer
	action, _ := SyncHosts(HostsSync{
		Path: path, Domain: "aip.local", Interactive: true, Out: &out,
		Consent: func(string) (bool, error) { return false, nil },
		Write:   func(string, []byte) error { t.Fatal("declined must not write"); return nil },
	})
	if action != HostsManual {
		t.Fatalf("expected manual, got %q", action)
	}
	if !strings.Contains(out.String(), "litellm.aip.local") {
		t.Fatalf("manual message missing:\n%s", out.String())
	}
}

func TestSyncHostsWriteFailureFallsBackToManual(t *testing.T) {
	path := tempHosts(t, "")
	var out bytes.Buffer
	action, _ := SyncHosts(HostsSync{
		Path: path, Domain: "aip.local", Interactive: true, Out: &out,
		Consent: func(string) (bool, error) { return true, nil },
		Write:   func(string, []byte) error { return errors.New("permission denied") },
	})
	if action != HostsManual {
		t.Fatalf("expected manual on write failure, got %q", action)
	}
	if !strings.Contains(out.String(), "litellm.aip.local") {
		t.Fatalf("manual message missing after write failure:\n%s", out.String())
	}
}

func TestRemoveHostsWritesWhenPresent(t *testing.T) {
	path := tempHosts(t, "")
	if _, err := hostsfile.Apply(path, Entries("aip.local")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var gotContent []byte
	action, _ := RemoveHosts(HostsSync{
		Path:  path,
		Write: func(_ string, content []byte) error { gotContent = content; return nil },
	})
	if action != HostsWritten {
		t.Fatalf("expected written (block removed), got %q", action)
	}
	if bytes.Contains(gotContent, []byte("litellm.aip.local")) {
		t.Fatalf("removed bytes must not contain the block:\n%s", gotContent)
	}
}

func TestRemoveHostsNoBlockIsUpToDate(t *testing.T) {
	path := tempHosts(t, "127.0.0.1\tlocalhost\n")
	action, _ := RemoveHosts(HostsSync{
		Path:  path,
		Write: func(string, []byte) error { t.Fatal("must not write when no block present"); return nil },
	})
	if action != HostsUpToDate {
		t.Fatalf("expected up-to-date, got %q", action)
	}
}

func TestHostsStatus(t *testing.T) {
	path := tempHosts(t, "")
	present, _, err := HostsStatus(path, "aip.local")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if present {
		t.Fatal("absent file should report present=false")
	}
	if _, err := hostsfile.Apply(path, Entries("aip.local")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	present, upToDate, _ := HostsStatus(path, "aip.local")
	if !present || !upToDate {
		t.Fatalf("expected present + up to date, got %v/%v", present, upToDate)
	}
}
