package secrets

import (
	"errors"

	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// ErrClawPatrolMissing is returned when the ClawPatrol gateway binary is not
// installed (→ exit 3). ErrPending marks operations whose ClawPatrol CLI wiring
// is completed during hardware bring-up (→ exit 4).
var (
	ErrClawPatrolMissing = errors.New("clawpatrol is not installed")
	ErrPending           = errors.New("clawpatrol credential operations are wired during hardware bring-up")
)

// realBroker fronts the ClawPatrol gateway. Credential values are handed to
// ClawPatrol and never written to platform disk. The concrete ClawPatrol CLI
// invocations are wired on a provisioned host (we do not invent them here), so
// these methods first verify the binary, then report ErrPending.
type realBroker struct {
	prober runtime.Prober
}

// RealBroker returns a Broker bound to the host's ClawPatrol gateway.
func RealBroker() Broker { return realBroker{prober: runtime.RealProber()} }

func (broker realBroker) ensureInstalled() error {
	if _, err := broker.prober.LookPath("clawpatrol"); err != nil {
		return ErrClawPatrolMissing
	}
	return nil
}

func (broker realBroker) Set(string, []byte) error {
	if err := broker.ensureInstalled(); err != nil {
		return err
	}
	return ErrPending
}

func (broker realBroker) Map(string, string) error {
	if err := broker.ensureInstalled(); err != nil {
		return err
	}
	return ErrPending
}

func (broker realBroker) Remove(string) error {
	if err := broker.ensureInstalled(); err != nil {
		return err
	}
	return ErrPending
}

func (broker realBroker) List() ([]Entry, error) {
	if err := broker.ensureInstalled(); err != nil {
		return nil, err
	}
	return nil, ErrPending
}
