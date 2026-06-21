package secrets

import (
	"errors"

	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// ErrGatewayMissing is returned when the LiteLLM gateway is not running, so its
// credential store cannot be reached (→ exit 3). ErrPending marks operations
// whose LiteLLM key-injection wiring is completed during hardware bring-up
// (→ exit 4).
var (
	ErrGatewayMissing = errors.New("litellm gateway is not running")
	ErrPending        = errors.New("litellm credential operations are wired during hardware bring-up")
)

// realBroker fronts the LiteLLM gateway's credential store (keys-in-LiteLLM).
// Credential values are handed to LiteLLM and never written to platform disk.
// The concrete LiteLLM key-injection is wired on a provisioned host (we do not
// invent it here), so these methods first verify the gateway, then report
// ErrPending.
type realBroker struct {
	prober runtime.Prober
}

// RealBroker returns a Broker bound to the host's LiteLLM gateway.
func RealBroker() Broker { return realBroker{prober: runtime.RealProber()} }

func (broker realBroker) ensureGateway() error {
	containerRuntime, err := runtime.ContainerRuntimeName(broker.prober)
	if err != nil {
		return ErrGatewayMissing
	}
	if _, err := broker.prober.Run(containerRuntime.Name, "inspect", "aip-litellm"); err != nil {
		return ErrGatewayMissing
	}
	return nil
}

func (broker realBroker) Set(string, []byte) error {
	if err := broker.ensureGateway(); err != nil {
		return err
	}
	return ErrPending
}

func (broker realBroker) Map(string, string) error {
	if err := broker.ensureGateway(); err != nil {
		return err
	}
	return ErrPending
}

func (broker realBroker) Remove(string) error {
	if err := broker.ensureGateway(); err != nil {
		return err
	}
	return ErrPending
}

func (broker realBroker) List() ([]Entry, error) {
	if err := broker.ensureGateway(); err != nil {
		return nil, err
	}
	return nil, ErrPending
}
