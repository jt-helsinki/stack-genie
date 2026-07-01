package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// relayNoiseLine matches the microsandbox agent-relay client connect/disconnect log
// lines — high-volume, low-signal churn (a client connects for an operation and
// disconnects when it finishes). The workspace log HIDES these by default; the `d`
// key toggles them back on for debugging.
func relayNoiseLine(line string) bool {
	return strings.Contains(line, "agent relay: client ")
}

// WorkspaceLogTailer returns the recent captured output of the CURRENT workspace
// microVM (msb logs --tail). Injected; the parent wires Manager.WorkspaceLogTail for
// the live current project.
type WorkspaceLogTailer = LogTextTailer

// WorkspaceRunning reports whether the CURRENT workspace microVM is running. The log
// is only fetched/shown while it is — a stopped workspace shows nothing rather than
// the stale captured output of a previous session. Injected; the parent wires it
// over the project's live status.
type WorkspaceRunning = LogSubjectRunning

// WorkspaceLog is the per-workspace embedded log: the generic LogView (logview.go)
// configured for a workspace microVM. It is embedded beneath the summary in the
// Workspace (project-detail) sub-tab — NOT a separate tab. The Services-tab detail
// view reuses the SAME LogView (configured for a container) for its embedded
// container log, so the streaming/scroll/freeze/normalize machinery lives in one
// place.
type WorkspaceLog = LogView

// WorkspaceLogFollowRequestedMsg asks the parent to open a live `msb logs -f` follow
// in the user's REAL terminal (via tea.ExecProcess) — a non-embedded view that
// renders natively (selectable, and animated in place when the captured stream has
// the control codes), restoring the TUI on exit.
type WorkspaceLogFollowRequestedMsg struct{ Project string }

// NewWorkspaceLog builds the workspace embedded log over the injected tailer, a
// running-check (the log is only fetched/shown while the workspace is running), and
// the current-project resolver. It configures the generic LogView with the
// workspace-specific labels + the `msb logs -f` follow message. When stream is
// non-nil (the SDK backend supports live streaming) the view STREAMS — loads history
// then appends new entries with no polling; when nil (the CLI backend) it falls back
// to the tailer poll.
func NewWorkspaceLog(tail WorkspaceLogTailer, running WorkspaceRunning, project func() string, stream LogStreamOpener) *WorkspaceLog {
	view := NewLogView(tail, running, project,
		LogViewLabels{
			NoSubject:  "no workspace selected — open one from the Workspaces view",
			NotRunning: "workspace not running — its log appears here while it is running (press s to start)",
			Loading:    "loading workspace log…",
			Empty:      "no workspace output captured yet — it streams in as the workspace runs",
		},
		func(subject string) tea.Msg { return WorkspaceLogFollowRequestedMsg{Project: subject} },
	)
	view.openStream = stream
	view.debugFilter = relayNoiseLine // hide the relay connect/disconnect churn (toggle: d)
	return view
}
