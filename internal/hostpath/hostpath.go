// Package hostpath normalizes host paths for the workspace microVM mount (arch
// §7, Slice 7). The microVM always runs Linux; on a Windows host the project
// source lives at a Windows path (e.g. C:\Users\me\app) which WSL2 exposes to the
// Linux side at /mnt/c/Users/me/app. Translating the host path to that mount form
// is the platform's responsibility before handing it to the sandbox. On macOS and
// Linux the host path is already POSIX and is used unchanged.
//
// This is a pure, OS-parameterized helper (GOOS is passed in), so it is
// unit-testable on any host — including translating Windows paths from a Mac.
package hostpath

import (
	"regexp"
	"strings"
)

// driveLetter matches a Windows absolute path with a drive letter, e.g.
// `C:\Users\me` or `D:/work` — capturing the drive and the remainder.
var driveLetter = regexp.MustCompile(`^([A-Za-z]):[\\/](.*)$`)

// ToWSLMount translates a Windows drive path to its WSL2 mount path
// (`C:\Users\me\app` → `/mnt/c/Users/me/app`). It returns (path, true) on a
// successful translation and (input, false) when the path is not a Windows
// drive path (already POSIX, UNC, or relative).
func ToWSLMount(windowsPath string) (string, bool) {
	match := driveLetter.FindStringSubmatch(windowsPath)
	if match == nil {
		return windowsPath, false
	}
	drive := strings.ToLower(match[1])
	rest := strings.ReplaceAll(match[2], `\`, "/")
	mount := "/mnt/" + drive
	if rest != "" {
		mount += "/" + rest
	}
	return mount, true
}

// WorkspaceMount returns the path to bind-mount the project source into the
// microVM, given the host GOOS. On Windows it is the WSL2 mount form; elsewhere
// the host path is returned unchanged.
func WorkspaceMount(goos, hostPath string) string {
	if goos == "windows" {
		if mount, ok := ToWSLMount(hostPath); ok {
			return mount
		}
	}
	return hostPath
}
