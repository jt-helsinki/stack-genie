package nono

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestRunOptionFlags(test *testing.T) {
	cases := []struct {
		name string
		opts RunOptions
		want []string
	}{
		{name: "empty", opts: RunOptions{}, want: nil},
		{name: "profile only", opts: RunOptions{Profile: "demo"}, want: []string{"--profile", "demo"}},
		{name: "name only", opts: RunOptions{Name: "claude"}, want: []string{"--name", "claude"}},
		{name: "allow-cwd only", opts: RunOptions{AllowCWD: true}, want: []string{"--allow-cwd"}},
		{
			name: "all three, in order",
			opts: RunOptions{Profile: "demo", Name: "claude", AllowCWD: true},
			want: []string{"--profile", "demo", "--name", "claude", "--allow-cwd"},
		},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			got := runOptionFlags(testCase.opts)
			if len(got) != len(testCase.want) {
				test.Fatalf("runOptionFlags() = %v, want %v", got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					test.Fatalf("runOptionFlags() = %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

func TestFakeRunnerRunDetachedDefaultSessionID(test *testing.T) {
	fake := &FakeRunner{}
	id, err := fake.RunDetached(context.Background(), RunOptions{Command: []string{"sleep", "60"}})
	if err != nil {
		test.Fatalf("RunDetached: %v", err)
	}
	if id != "fake-session" {
		test.Errorf("id = %q, want fake-session", id)
	}
	if len(fake.RunDetachedCalls) != 1 {
		test.Fatalf("RunDetachedCalls = %d, want 1", len(fake.RunDetachedCalls))
	}
}

func TestFakeRunnerRunDetachedCustomSessionID(test *testing.T) {
	fake := &FakeRunner{RunDetachedSessionID: "abc123"}
	id, err := fake.RunDetached(context.Background(), RunOptions{})
	if err != nil {
		test.Fatalf("RunDetached: %v", err)
	}
	if id != "abc123" {
		test.Errorf("id = %q, want abc123", id)
	}
}

func TestFakeRunnerRunDetachedErr(test *testing.T) {
	wantErr := errors.New("boom")
	fake := &FakeRunner{RunDetachedErr: wantErr}
	_, err := fake.RunDetached(context.Background(), RunOptions{})
	if !errors.Is(err, wantErr) {
		test.Errorf("err = %v, want %v", err, wantErr)
	}
}

func TestFakeRunnerInspectNotFound(test *testing.T) {
	fake := &FakeRunner{}
	_, err := fake.Inspect(context.Background(), "missing")
	if !errors.Is(err, ErrSessionNotFound) {
		test.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestFakeRunnerInspectFound(test *testing.T) {
	fake := &FakeRunner{InspectSessions: map[string]Session{
		"abc123": {SessionID: "abc123", Status: "running"},
	}}
	session, err := fake.Inspect(context.Background(), "abc123")
	if err != nil {
		test.Fatalf("Inspect: %v", err)
	}
	if session.Status != "running" {
		test.Errorf("Status = %q, want running", session.Status)
	}
}

func TestFakeRunnerStopRecordsForce(test *testing.T) {
	fake := &FakeRunner{}
	if err := fake.Stop(context.Background(), "abc123", true); err != nil {
		test.Fatalf("Stop: %v", err)
	}
	if len(fake.StopCalls) != 1 || !fake.StopCalls[0].Force {
		test.Errorf("StopCalls = %+v, want one call with Force=true", fake.StopCalls)
	}
}

func TestFakeRunnerListSessions(test *testing.T) {
	fake := &FakeRunner{Sessions: []Session{{SessionID: "a"}, {SessionID: "b"}}}
	sessions, err := fake.ListSessions(context.Background(), true)
	if err != nil {
		test.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		test.Fatalf("len(sessions) = %d, want 2", len(sessions))
	}
	if len(fake.ListSessionsCalls) != 1 || !fake.ListSessionsCalls[0] {
		test.Errorf("ListSessionsCalls = %v, want [true]", fake.ListSessionsCalls)
	}
}

// TestRealRunnerLiveRoundTrip exercises RealRunner against an actual `nono`
// binary: start a detached session, list it, inspect it, stop it. Gated
// behind AIP_HARDWARE_TESTS like the rest of this project's live-tool tests
// (see internal/workspace/workspace_live_test.go) since it needs nono
// installed and actually launches a real sandboxed process.
//
// Run with: AIP_HARDWARE_TESTS=1 go test ./internal/nono/ -run TestRealRunnerLiveRoundTrip -v
func TestRealRunnerLiveRoundTrip(test *testing.T) {
	if os.Getenv("AIP_HARDWARE_TESTS") == "" {
		test.Skip("set AIP_HARDWARE_TESTS=1 to run the live nono test (needs the nono CLI installed)")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		test.Skip("nono not installed, skipping")
	}

	runner := RealRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := test.TempDir()
	sessionID, err := runner.RunDetached(ctx, RunOptions{AllowCWD: true, Dir: dir, Command: []string{"sleep", "30"}})
	if err != nil {
		test.Fatalf("RunDetached: %v", err)
	}
	defer func() { _ = runner.Stop(context.Background(), sessionID, true) }()

	sessions, err := runner.ListSessions(ctx, false)
	if err != nil {
		test.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, session := range sessions {
		if session.SessionID == sessionID {
			found = true
		}
	}
	if !found {
		test.Errorf("session %s not found in ListSessions() = %+v", sessionID, sessions)
	}

	inspected, err := runner.Inspect(ctx, sessionID)
	if err != nil {
		test.Fatalf("Inspect: %v", err)
	}
	if inspected.Status != "running" {
		test.Errorf("Status = %q, want running", inspected.Status)
	}

	if err := runner.Stop(ctx, sessionID, false); err != nil {
		test.Fatalf("Stop: %v", err)
	}
}
