package vllm

import "sync"

// FakeRunner is an in-memory Runner for tests. It records every Start/Stop, hands
// out incrementing PIDs, and can be made to fail via the *Err fields. It never
// touches a real process. Safe for concurrent use.
type FakeRunner struct {
	mu sync.Mutex

	nextPID int

	// StartErr, when non-nil, fails every Start. StartErrs (keyed by alias) overrides
	// it for specific aliases.
	StartErr  error
	StartErrs map[string]error
	// StopErr, when non-nil, fails every Stop.
	StopErr error

	// StartCalls / StopCalls record invocations in order.
	StartCalls []FakeStartCall
	StopCalls  []ServerHandle
}

// FakeStartCall captures the arguments of one Runner.Start invocation.
type FakeStartCall struct {
	Alias    string
	Model    string
	Port     int
	StoreDir string
	Opts     ServeOptions
	Handle   ServerHandle
}

// Start records the call, returns a fresh handle, and honours the configured errors.
func (fake *FakeRunner) Start(alias, model string, port int, storeDir string, opts ServeOptions) (ServerHandle, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.StartErrs != nil {
		if err, ok := fake.StartErrs[alias]; ok && err != nil {
			return ServerHandle{}, err
		}
	}
	if fake.StartErr != nil {
		return ServerHandle{}, fake.StartErr
	}
	fake.nextPID++
	handle := ServerHandle{PID: fake.nextPID}
	fake.StartCalls = append(fake.StartCalls, FakeStartCall{
		Alias: alias, Model: model, Port: port, StoreDir: storeDir, Opts: opts, Handle: handle,
	})
	return handle, nil
}

// Stop records the handle and honours StopErr.
func (fake *FakeRunner) Stop(handle ServerHandle) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.StopCalls = append(fake.StopCalls, handle)
	return fake.StopErr
}

// StartCount returns how many Start calls were recorded.
func (fake *FakeRunner) StartCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.StartCalls)
}

// StopCount returns how many Stop calls were recorded.
func (fake *FakeRunner) StopCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.StopCalls)
}
