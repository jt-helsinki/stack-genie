package views

import (
	"context"
	"io"
	"strings"
	"testing"
)

// fakeLogStream is a scripted LogStream: it returns each chunk in turn, then io.EOF.
type fakeLogStream struct {
	chunks []string
	index  int
	closed bool
}

func (stream *fakeLogStream) Recv(ctx context.Context) (string, error) {
	if stream.index >= len(stream.chunks) {
		return "", io.EOF
	}
	chunk := stream.chunks[stream.index]
	stream.index++
	return chunk, nil
}

func (stream *fakeLogStream) Close() error {
	stream.closed = true
	return nil
}

// TestLogViewStreamsHistoryThenAppends drives the streaming path synchronously
// (executing each returned command and feeding its message back, like the bubbletea
// loop) and asserts history loads, chunks accumulate, and EOF ends cleanly.
func TestLogViewStreamsHistoryThenAppends(test *testing.T) {
	stream := &fakeLogStream{chunks: []string{"history line\n", "live line\n"}}
	opener := func(ctx context.Context) (LogStream, error) { return stream, nil }
	view := NewWorkspaceLog(
		func() (string, error) { return "", nil }, // tailer unused in stream mode
		func() bool { return true },
		func() string { return "demo" },
		opener,
	)
	view.SetSize(80, 10)

	// Init returns the open-stream command (streaming mode, visible, running).
	openCmd := view.Init()
	if openCmd == nil {
		test.Fatal("Init in streaming mode should return an open-stream command")
	}
	recvCmd := view.Update(openCmd())
	if view.streamHandle == nil {
		test.Fatal("expected the live stream handle to be stored after opening")
	}
	if recvCmd == nil {
		test.Fatal("opening the stream should arm the first Recv")
	}

	// First chunk: history.
	recvCmd = view.Update(recvCmd())
	if !strings.Contains(view.streamBuf, "history line") {
		test.Fatalf("expected history to accumulate, buffer=%q", view.streamBuf)
	}
	if !view.loaded {
		test.Error("view should be loaded after the first chunk")
	}

	// Second chunk: a live entry appends to the same buffer (scrollback preserved).
	recvCmd = view.Update(recvCmd())
	if !strings.Contains(view.streamBuf, "history line") || !strings.Contains(view.streamBuf, "live line") {
		test.Fatalf("expected both entries in the buffer, got %q", view.streamBuf)
	}
	if !strings.Contains(view.View(), "live line") {
		test.Errorf("rendered view should show the streamed content:\n%s", view.View())
	}

	// Next Recv yields EOF — a normal ending, not surfaced as an error.
	view.Update(recvCmd())
	if view.err != nil {
		test.Errorf("EOF should end the stream cleanly, not surface an error: %v", view.err)
	}
}

// TestLogViewStreamStaleChunkIgnored verifies a chunk tagged with an old generation
// (from a previous activation/subject) is discarded.
func TestLogViewStreamStaleChunkIgnored(test *testing.T) {
	view := NewWorkspaceLog(
		func() (string, error) { return "", nil },
		func() bool { return true },
		func() string { return "demo" },
		func(ctx context.Context) (LogStream, error) { return &fakeLogStream{}, nil },
	)
	view.generation = 7
	view.Update(logViewStreamChunkMsg{raw: "stale", content: "stale", generation: 6})
	if strings.Contains(view.streamBuf, "stale") {
		test.Error("a stale-generation chunk must be ignored")
	}
}

// TestLogViewStaleStreamOpenedClosed verifies an opened stream that arrives for an old
// generation is closed immediately (no leak).
func TestLogViewStaleStreamOpenedClosed(test *testing.T) {
	view := NewWorkspaceLog(
		func() (string, error) { return "", nil },
		func() bool { return true },
		func() string { return "demo" },
		func(ctx context.Context) (LogStream, error) { return &fakeLogStream{}, nil },
	)
	view.generation = 3
	stale := &fakeLogStream{}
	_, cancel := context.WithCancel(context.Background())
	view.Update(logViewStreamOpenedMsg{stream: stale, cancel: cancel, generation: 2})
	if !stale.closed {
		test.Error("a stale opened stream must be closed to avoid a leak")
	}
	if view.streamHandle != nil {
		test.Error("a stale opened stream must not become the active handle")
	}
}
