package apps

import (
	"errors"
	"testing"
)

func TestAllocatePortSkipsReserved(test *testing.T) {
	reserved := map[int]bool{portRangeStart: true, portRangeStart + 1: true}
	port, err := allocatePort(reserved, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if port != portRangeStart+2 {
		test.Fatalf("allocated %d, want %d (first unreserved)", port, portRangeStart+2)
	}
}

// TestSuggestedHostPortDistinctWhenDefaultsBusy mirrors the create wizard's seed loop: it
// calls SuggestedHostPort for several apps while RESERVING each result before the next, with
// every app's familiar container port busy on the host. Each app must get a DISTINCT free
// port — the bug being that, without reserving between calls, they all fell back to the same
// auto-allocated port.
func TestSuggestedHostPortDistinctWhenDefaultsBusy(test *testing.T) {
	// Every known app's familiar container port is busy; window ports are free.
	busy := map[int]bool{}
	for _, manifest := range All() {
		busy[manifest.ContainerPort] = true
	}
	isFree := func(port int) bool { return !busy[port] }

	reserved := map[int]bool{}
	seen := map[int]bool{}
	for _, manifest := range All() {
		port := SuggestedHostPort(manifest.Key, reserved, isFree)
		if port == 0 {
			test.Fatalf("no port suggested for %q", manifest.Key)
		}
		if !isFree(port) {
			test.Errorf("%q suggested busy host port %d", manifest.Key, port)
		}
		if seen[port] {
			test.Errorf("%q reused already-seeded port %d (ports must be distinct)", manifest.Key, port)
		}
		seen[port] = true
		reserved[port] = true // mirror the wizard reserving each seed
	}
}

func TestAllocatePortSkipsBusy(test *testing.T) {
	// portRangeStart is "busy" (not free); the next one is free.
	isFree := func(port int) bool { return port != portRangeStart }
	port, err := allocatePort(nil, isFree)
	if err != nil {
		test.Fatal(err)
	}
	if port != portRangeStart+1 {
		test.Fatalf("allocated %d, want %d (first free)", port, portRangeStart+1)
	}
}

func TestAllocatePortExhausted(test *testing.T) {
	_, err := allocatePort(nil, func(int) bool { return false })
	if !errors.Is(err, ErrNoFreePort) {
		test.Fatalf("err = %v, want ErrNoFreePort", err)
	}
}

func TestAllocateEntriesUnique(test *testing.T) {
	entries, err := AllocateEntries([]string{"openwebui", "anythingllm"}, nil, nil, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 2 {
		test.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Port == entries[1].Port {
		test.Fatalf("entries collided on port %d", entries[0].Port)
	}
	if entries[0].Key != "openwebui" || entries[1].Key != "anythingllm" {
		test.Fatalf("keys = %q,%q", entries[0].Key, entries[1].Key)
	}
}

func TestAllocateEntriesAvoidsReserved(test *testing.T) {
	reserved := map[int]bool{portRangeStart: true}
	entries, err := AllocateEntries([]string{"openwebui"}, nil, reserved, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if entries[0].Port == portRangeStart {
		test.Fatalf("allocated a reserved port %d", entries[0].Port)
	}
}

func TestAllocateEntriesSkipsUnknown(test *testing.T) {
	entries, err := AllocateEntries([]string{"openwebui", "nope"}, nil, nil, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != "openwebui" {
		test.Fatalf("entries = %v, want only openwebui", entries)
	}
}

// A user-requested port is honored verbatim (create prompt / --app-port).
func TestAllocateEntriesHonorsRequestedPort(test *testing.T) {
	entries, err := AllocateEntries([]string{"openwebui", "anythingllm"},
		map[string]int{"openwebui": 8080}, nil, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if entries[0].Key != "openwebui" || entries[0].Port != 8080 {
		test.Fatalf("openwebui port = %d, want the requested 8080", entries[0].Port)
	}
	// The un-requested app is still auto-allocated (and must not collide with 8080).
	if entries[1].Port == 8080 {
		test.Fatalf("auto-allocated port collided with the requested 8080")
	}
}

// A requested port already reserved by another workspace/app is rejected.
func TestAllocateEntriesRejectsReservedRequestedPort(test *testing.T) {
	_, err := AllocateEntries([]string{"openwebui"},
		map[string]int{"openwebui": 9000}, map[int]bool{9000: true}, func(int) bool { return true })
	if !errors.Is(err, ErrPortUnavailable) {
		test.Fatalf("err = %v, want ErrPortUnavailable for a reserved requested port", err)
	}
}

// A requested port already in use on the host is rejected.
func TestAllocateEntriesRejectsBusyRequestedPort(test *testing.T) {
	_, err := AllocateEntries([]string{"openwebui"},
		map[string]int{"openwebui": 8080}, nil, func(int) bool { return false })
	if !errors.Is(err, ErrPortUnavailable) {
		test.Fatalf("err = %v, want ErrPortUnavailable for a busy requested port", err)
	}
}

// Two selected apps requesting the SAME port collide.
func TestAllocateEntriesRejectsRequestedCollision(test *testing.T) {
	_, err := AllocateEntries([]string{"openwebui", "anythingllm"},
		map[string]int{"openwebui": 8080, "anythingllm": 8080}, nil, func(int) bool { return true })
	if !errors.Is(err, ErrPortUnavailable) {
		test.Fatalf("err = %v, want ErrPortUnavailable for two apps on the same port", err)
	}
}

func TestSuggestedHostPort(test *testing.T) {
	// Free + unreserved → the app's familiar container port.
	if port := SuggestedHostPort("openwebui", nil, func(int) bool { return true }); port != openWebUIPortGuest {
		test.Errorf("suggested port = %d, want the container port %d", port, openWebUIPortGuest)
	}
	// Container port reserved → falls back to an auto-allocated window port.
	port := SuggestedHostPort("openwebui", map[int]bool{openWebUIPortGuest: true}, func(int) bool { return true })
	if port < portRangeStart || port > portRangeEnd {
		test.Errorf("fallback suggested port = %d, want one in the allocation window", port)
	}
	if got := SuggestedHostPort("nope", nil, func(int) bool { return true }); got != 0 {
		test.Errorf("unknown key suggested port = %d, want 0", got)
	}
}

func TestAllocateEntriesEmpty(test *testing.T) {
	entries, err := AllocateEntries(nil, nil, nil, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 0 {
		test.Fatalf("entries = %v, want empty for no apps", entries)
	}
}
