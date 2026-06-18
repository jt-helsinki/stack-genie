// Package git is a thin wrapper over the git subprocess for the operations the
// platform needs at project creation (arch §21 is the agent-facing git system;
// this is just init/clone). It is an interface so the project flow can be tested
// with a fake.
package git

import (
	"fmt"
	"os/exec"
)

// Runner performs git operations.
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
