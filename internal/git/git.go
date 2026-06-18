// Package git is a thin wrapper over the git subprocess for the only git
// operations the platform performs itself: initializing or cloning a project's
// repository at creation. Everything else — branches, commits, merges, rebases —
// is the in-workspace agent's job, not the platform's (arch §21).
package git

import (
	"fmt"
	"os/exec"
)

// Runner performs the platform's git operations.
type Runner interface {
	Init(dir string) error
	Clone(repo, dir string) error
}

type execRunner struct{}

// RealRunner shells out to the host's git.
func RealRunner() Runner { return execRunner{} }

func (execRunner) Init(dir string) error        { return run("init", dir) }
func (execRunner) Clone(repo, dir string) error { return run("clone", repo, dir) }

func run(args ...string) error {
	output, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, output)
	}
	return nil
}
