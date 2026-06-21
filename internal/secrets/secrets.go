// Package secrets is the platform's thin front to the LiteLLM gateway's
// credential store — keys-in-LiteLLM (CLI §16.1). Real provider API keys live in
// the LiteLLM container (set as env passthrough at LiteLLM launch, resolved by
// LiteLLM's os.environ/<PROVIDER>_API_KEY placeholders). The platform never
// writes secret values to its own disk: values flow straight to LiteLLM via the
// Broker, and the Entry type carries names/metadata only — there is no Value
// field to leak. The workspace agent holds only a scoped LiteLLM virtual key,
// never a provider secret.
package secrets

// Entry is credential metadata for `ai secrets list`. It deliberately has NO
// value field, so a list/JSON envelope can never carry secret material.
type Entry struct {
	Name   string `json:"name"`
	EnvVar string `json:"env_var,omitempty"` // workspace placeholder the gateway swaps
}

// Broker is the LiteLLM gateway's credential store (keys-in-LiteLLM). The real
// impl injects keys into the LiteLLM container; tests use a fake.
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
