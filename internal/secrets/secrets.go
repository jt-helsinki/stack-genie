// Package secrets is the platform's thin front to ClawPatrol's credential store
// (arch §17, CLI §16.1). The platform never writes secret values to its own
// disk: values flow straight into ClawPatrol's SQLite via the Broker, and the
// Entry type carries names/metadata only — there is no Value field to leak.
package secrets

// Entry is credential metadata for `ai secrets list`. It deliberately has NO
// value field, so a list/JSON envelope can never carry secret material.
type Entry struct {
	Name   string `json:"name"`
	EnvVar string `json:"env_var,omitempty"` // workspace placeholder the gateway swaps
}

// Broker is ClawPatrol's credential store. The real impl shells out to the
// ClawPatrol gateway; tests use a fake.
type Broker interface {
	// Set stores (or replaces) a credential by name. value is never persisted to
	// platform disk by the caller; the broker owns it.
	Set(name string, value []byte) error
	// Map binds a stored credential to a workspace placeholder env var.
	Map(name, envVar string) error
	// Remove deletes a credential.
	Remove(name string) error
	// List returns names + metadata only — never values.
	List() ([]Entry, error)
}
