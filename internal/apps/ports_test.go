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
	entries, err := AllocateEntries([]string{"openwebui", "anythingllm"}, nil, func(int) bool { return true })
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
	entries, err := AllocateEntries([]string{"openwebui"}, reserved, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if entries[0].Port == portRangeStart {
		test.Fatalf("allocated a reserved port %d", entries[0].Port)
	}
}

func TestAllocateEntriesSkipsUnknown(test *testing.T) {
	entries, err := AllocateEntries([]string{"openwebui", "nope"}, nil, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != "openwebui" {
		test.Fatalf("entries = %v, want only openwebui", entries)
	}
}

func TestAllocateEntriesEmpty(test *testing.T) {
	entries, err := AllocateEntries(nil, nil, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 0 {
		test.Fatalf("entries = %v, want empty for no apps", entries)
	}
}
