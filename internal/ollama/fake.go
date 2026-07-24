package ollama

// Fake is an in-memory Client for tests (CLI and others). Set the *Func fields to
// control behavior; the zero value returns empty/nil. Pull replays PullFrames
// through the callback before returning PullErr.
type Fake struct {
	ListModels []Model
	ListErr    error

	PullFrames  []PullProgress
	PullErr     error
	PulledName  string           // captures the last name passed to Pull
	PulledNames []string         // captures every name passed to Pull, in order
	PullErrs    map[string]error // per-name Pull error (overrides PullErr when the name has an entry)

	RemoveErr   error
	RemovedName string // captures the last name passed to Remove
	ShowInfo    ModelInfo
	ShowErr     error
	ShownName   string // captures the last name passed to Show

	SetNumCtxErr   error
	SetNumCtxCalls []SetNumCtxCall // captures every (name, numCtx) passed to SetNumCtx, in order
}

// SetNumCtxCall records one SetNumCtx invocation so a test can assert baking.
type SetNumCtxCall struct {
	Name   string
	NumCtx int
}

func (fake *Fake) List() ([]Model, error) { return fake.ListModels, fake.ListErr }

func (fake *Fake) Pull(name string, progress func(PullProgress)) error {
	fake.PulledName = name
	fake.PulledNames = append(fake.PulledNames, name)
	if progress != nil {
		for _, frame := range fake.PullFrames {
			progress(frame)
		}
	}
	if fake.PullErrs != nil {
		if err, ok := fake.PullErrs[name]; ok {
			return err
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

func (fake *Fake) SetNumCtx(name string, numCtx int) error {
	fake.SetNumCtxCalls = append(fake.SetNumCtxCalls, SetNumCtxCall{Name: name, NumCtx: numCtx})
	return fake.SetNumCtxErr
}
