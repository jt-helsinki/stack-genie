package ollama

// Fake is an in-memory Client for tests (CLI and others). Set the *Func fields to
// control behavior; the zero value returns empty/nil. Pull replays PullFrames
// through the callback before returning PullErr.
type Fake struct {
	ListModels []Model
	ListErr    error

	PullFrames []PullProgress
	PullErr    error
	PulledName string // captures the last name passed to Pull

	RemoveErr   error
	RemovedName string // captures the last name passed to Remove
	ShowInfo    ModelInfo
	ShowErr     error
	ShownName   string // captures the last name passed to Show
}

func (fake *Fake) List() ([]Model, error) { return fake.ListModels, fake.ListErr }

func (fake *Fake) Pull(name string, progress func(PullProgress)) error {
	fake.PulledName = name
	if progress != nil {
		for _, frame := range fake.PullFrames {
			progress(frame)
		}
	}
	return fake.PullErr
}

func (fake *Fake) Remove(name string) error {
	fake.RemovedName = name
	return fake.RemoveErr
}

func (fake *Fake) Show(name string) (ModelInfo, error) {
	fake.ShownName = name
	return fake.ShowInfo, fake.ShowErr
}
