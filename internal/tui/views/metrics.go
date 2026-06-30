package views

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
)

// MetricsStream is a live metrics subscription: Recv blocks for the next sample, Close
// stops it. Satisfied structurally by workspace.MetricsStream.
type MetricsStream interface {
	Recv(ctx context.Context) (workspace.MetricsSnapshot, error)
	Close() error
}

// MetricsStreamOpener opens a metrics stream for the CURRENT workspace. nil → the view
// reports that metrics are unavailable on this backend.
type MetricsStreamOpener func(ctx context.Context) (MetricsStream, error)

// metricsHeadingRows is the height reserved above the metrics table (status + blank).
const metricsHeadingRows = 2

type metricsOpenedMsg struct {
	stream     MetricsStream
	ctx        context.Context
	cancel     context.CancelFunc
	err        error
	generation int
}

type metricsSampleMsg struct {
	snapshot   workspace.MetricsSnapshot
	err        error
	generation int
}

// Metrics is the per-workspace embedded metrics view: it opens one live
// MetricsStream and renders each sample in a table (Metric · Value). It streams (no
// polling) — re-armed Recv commands with a generation guard + cancelable ctx, torn
// down on sub-tab switch (SetActive) or workspace switch (Init).
type Metrics struct {
	open    MetricsStreamOpener
	running func() bool
	subject func() string

	table      listTable
	loaded     bool
	hasData    bool
	notRunning bool
	err        error

	width      int
	height     int
	generation int
	paused     bool

	streamHandle MetricsStream
	streamCtx    context.Context
	streamCancel context.CancelFunc
}

// NewMetrics builds the metrics view over a stream opener, a running-check (metrics
// need a running workspace) and the current-workspace resolver.
func NewMetrics(open MetricsStreamOpener, running func() bool, subject func() string) *Metrics {
	columns := []listColumn{{title: "Metric", width: 16}, {title: "Value", width: 40}}
	return &Metrics{open: open, running: running, subject: subject, table: newListTable(columns)}
}

func (view *Metrics) Title() string { return "Metrics" }
func (view *Metrics) Hints() string { return "↑/↓ scroll · r refresh" }

func (view *Metrics) SetSize(width, height int) {
	view.width, view.height = width, height
	tableHeight := height - metricsHeadingRows
	if tableHeight < 1 {
		tableHeight = 1
	}
	view.table.SetSize(width, tableHeight)
}

// SetActive pauses/closes the stream while the sub-tab is not visible.
func (view *Metrics) SetActive(active bool) {
	view.paused = !active
	if !active {
		view.closeStream()
	}
}

func (view *Metrics) closeStream() {
	if view.streamCancel != nil {
		view.streamCancel()
		view.streamCancel = nil
	}
	view.streamCtx = nil
	if view.streamHandle != nil {
		_ = view.streamHandle.Close()
		view.streamHandle = nil
	}
}

// Init opens a fresh stream for the current workspace (if running + visible).
func (view *Metrics) Init() tea.Cmd {
	view.generation++
	view.closeStream()
	view.loaded = false
	view.hasData = false
	view.notRunning = false
	view.err = nil
	if view.subject == nil || view.subject() == "" {
		return nil
	}
	if view.open == nil {
		view.loaded = true
		view.err = errors.New("metrics are not available on this workspace backend")
		return nil
	}
	if view.running != nil && !view.running() {
		view.loaded = true
		view.notRunning = true
		return nil
	}
	if view.paused {
		return nil
	}
	return view.openStreamCmd(view.generation)
}

func (view *Metrics) openStreamCmd(generation int) tea.Cmd {
	open := view.open
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := open(ctx)
		if err != nil {
			cancel()
			return metricsOpenedMsg{err: err, generation: generation}
		}
		return metricsOpenedMsg{stream: stream, ctx: ctx, cancel: cancel, generation: generation}
	}
}

func (view *Metrics) recvCmd(stream MetricsStream, ctx context.Context, generation int) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := stream.Recv(ctx)
		return metricsSampleMsg{snapshot: snapshot, err: err, generation: generation}
	}
}

func (view *Metrics) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case metricsOpenedMsg:
		if message.generation != view.generation {
			if message.cancel != nil {
				message.cancel()
			}
			if message.stream != nil {
				_ = message.stream.Close()
			}
			return nil
		}
		view.loaded = true
		if message.err != nil {
			view.err = message.err
			return nil
		}
		view.err = nil
		view.streamHandle = message.stream
		view.streamCtx = message.ctx
		view.streamCancel = message.cancel
		return view.recvCmd(message.stream, message.ctx, view.generation)
	case metricsSampleMsg:
		if message.generation != view.generation {
			return nil
		}
		view.loaded = true
		if message.err != nil {
			// EOF / cancel are normal endings (workspace stopped / we switched away);
			// only a real error surfaces. The last sample stays on screen.
			if !errors.Is(message.err, io.EOF) && !errors.Is(message.err, context.Canceled) {
				view.err = message.err
			}
			return nil
		}
		view.hasData = true
		view.err = nil
		view.table.SetRows(metricsRows(message.snapshot))
		return view.recvCmd(view.streamHandle, view.streamCtx, view.generation)
	case tea.KeyMsg:
		if message.String() == "r" {
			return view.Init()
		}
	}
	return view.table.Update(msg)
}

func (view *Metrics) View() string {
	if view.subject == nil || view.subject() == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view")
	}
	if view.notRunning {
		return ui.Muted.Render("workspace not running — live metrics appear here while it runs (press s to start)")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded || !view.hasData {
		return ui.Muted.Render("collecting live metrics…")
	}
	return ui.Muted.Render("live — updates every 2s") + "\n\n" + view.table.View()
}

// metricsRows renders one snapshot as table rows.
func metricsRows(snapshot workspace.MetricsSnapshot) [][]string {
	return [][]string{
		{"CPU", fmt.Sprintf("%.1f%%", snapshot.CPUPercent)},
		{"Memory", humanBytes(snapshot.MemoryBytes) + " / " + humanBytes(snapshot.MemoryLimitBytes)},
		{"Disk read", humanBytes(snapshot.DiskReadBytes)},
		{"Disk write", humanBytes(snapshot.DiskWriteBytes)},
		{"Net RX", humanBytes(snapshot.NetRxBytes)},
		{"Net TX", humanBytes(snapshot.NetTxBytes)},
		{"Uptime", snapshot.Uptime.Round(time.Second).String()},
	}
}

// humanBytes formats a byte count with a binary (KiB/MiB/…) suffix.
func humanBytes(count uint64) string {
	const unit = 1024
	if count < unit {
		return fmt.Sprintf("%d B", count)
	}
	div, exp := uint64(unit), 0
	for value := count / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(count)/float64(div), "KMGTPE"[exp])
}
