package views

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/workspace"
)

type fakeMetricsStream struct {
	samples []workspace.MetricsSnapshot
	index   int
	closed  bool
}

func (stream *fakeMetricsStream) Recv(ctx context.Context) (workspace.MetricsSnapshot, error) {
	if stream.index >= len(stream.samples) {
		return workspace.MetricsSnapshot{}, io.EOF
	}
	sample := stream.samples[stream.index]
	stream.index++
	return sample, nil
}

func (stream *fakeMetricsStream) Close() error {
	stream.closed = true
	return nil
}

// TestMetricsStreamsIntoTable drives the metrics streaming path synchronously and
// asserts a sample is rendered into the table.
func TestMetricsStreamsIntoTable(test *testing.T) {
	stream := &fakeMetricsStream{samples: []workspace.MetricsSnapshot{
		{CPUPercent: 12.5, MemoryBytes: 512 * 1024 * 1024, MemoryLimitBytes: 4096 * 1024 * 1024, Uptime: 90 * time.Second},
	}}
	view := NewMetrics(
		func(ctx context.Context) (MetricsStream, error) { return stream, nil },
		func() bool { return true },
		func() string { return "demo" },
	)
	view.SetSize(80, 12)

	openCmd := view.Init()
	if openCmd == nil {
		test.Fatal("Init should open a metrics stream")
	}
	recvCmd := view.Update(openCmd())
	if view.streamHandle == nil || recvCmd == nil {
		test.Fatal("opening should store the handle and arm the first Recv")
	}
	// First sample → table rows.
	recvCmd = view.Update(recvCmd())
	if !view.hasData {
		test.Fatal("expected a sample to be recorded")
	}
	out := view.View()
	if !strings.Contains(out, "CPU") || !strings.Contains(out, "12.5%") {
		test.Errorf("expected CPU metric in the table:\n%s", out)
	}
	if !strings.Contains(out, "Uptime") {
		test.Errorf("expected Uptime metric in the table:\n%s", out)
	}
	// EOF ends the stream cleanly (no error surfaced).
	view.Update(recvCmd())
	if view.err != nil {
		test.Errorf("EOF should not surface as an error: %v", view.err)
	}
}

// TestMetricsNotRunning shows the not-running hint and opens no stream.
func TestMetricsNotRunning(test *testing.T) {
	view := NewMetrics(
		func(ctx context.Context) (MetricsStream, error) { return &fakeMetricsStream{}, nil },
		func() bool { return false },
		func() string { return "demo" },
	)
	if cmd := view.Init(); cmd != nil {
		test.Error("Init on a not-running workspace should not open a stream")
	}
	if !strings.Contains(view.View(), "not running") {
		test.Errorf("expected the not-running hint:\n%s", view.View())
	}
}

// TestMetricsUnsupportedBackend: a nil opener reports metrics unavailable.
func TestMetricsUnsupportedBackend(test *testing.T) {
	view := NewMetrics(nil, func() bool { return true }, func() string { return "demo" })
	view.Init()
	if !strings.Contains(view.View(), "not available") {
		test.Errorf("expected an unavailable hint with no opener:\n%s", view.View())
	}
}
