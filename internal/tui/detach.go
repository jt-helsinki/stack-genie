package tui

import "syscall"

// detachedSysProcAttr starts a child in its OWN session (setsid), detaching it from
// `ai ui`'s controlling terminal and process group. A detached lifecycle action
// (`ai start`/`stop`/`restart`) therefore keeps running even after `ai ui` exits.
// Supported hosts are macOS and Linux, where Setsid is available.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
