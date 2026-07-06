package views

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestLogViewStreamOpenErrorFallsBackToTailer: when the live stream can't open (e.g.
// the microVM doesn't exist yet — the image is still building), the view must NOT
// surface a hard error; it degrades to the tailer poll (which shows the build log and,
// once the VM is up, the microVM log), returning a follow-up command.
func TestLogViewStreamOpenErrorFallsBackToTailer(test *testing.T) {
	opener := func(ctx context.Context) (LogStream, error) { return nil, errors.New("no sandbox yet") }
	view := NewWorkspaceLog(
		func() (string, error) { return "build log line\n", nil },
		func() bool { return true },
		func() string { return "demo" },
		opener,
	)
	view.SetSize(80, 10)

	openCmd := view.Init()
	if openCmd == nil {
		test.Fatal("Init in streaming mode should return an open-stream command")
	}
	fallbackCmd := view.Update(openCmd())
	if view.err != nil {
		test.Fatalf("a stream-open failure must not surface as a hard error: %v", view.err)
	}
	if fallbackCmd == nil {
		test.Fatal("a stream-open failure must fall back to a tailer poll (non-nil cmd)")
	}
}

// TestLogViewSetStreamingEnabledFallsBackToTailer: disabling streaming (as the app
// does for the duration of a start/restart) makes the view use the POLL path so it
// tails the live BUILD LOG rather than the microVM stream (which during a restart
// shows the old VM shutting down and stalls). Re-enabling restores the stream.
func TestLogViewSetStreamingEnabledFallsBackToTailer(test *testing.T) {
	opener := func(ctx context.Context) (LogStream, error) {
		return &fakeLogStream{chunks: []string{"vm log\n"}}, nil
	}
	view := NewWorkspaceLog(
		func() (string, error) { return "=== ai restart demo ===\ninstalling…\n", nil },
		func() bool { return true },
		func() string { return "demo" },
		opener,
	)
	view.SetSize(80, 10)

	// Configured for streaming by default.
	if !view.streaming() {
		test.Fatal("a stream-configured view must start in streaming mode")
	}
	// Disable: Init must now take the POLL path (issue a tailer load + tick), NOT open
	// a stream — so the build log is what renders.
	view.SetStreamingEnabled(false)
	if view.streaming() {
		test.Fatal("SetStreamingEnabled(false) must leave streaming() false")
	}
	if view.Init() == nil {
		test.Fatal("poll mode Init should return a load+tick batch")
	}
	// Drive the tailer load (same generation as Init) and feed its result back: the
	// build log must render, proving the poll path (not the stream) is in use.
	view.Update(view.issueLoad()())
	if !strings.Contains(view.content, "installing") {
		test.Errorf("with streaming disabled the build log must render, got %q", view.content)
	}
	// Re-enable: streaming resumes for the next activation.
	view.SetStreamingEnabled(true)
	if !view.streaming() {
		test.Fatal("SetStreamingEnabled(true) must restore streaming mode")
	}
}

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
