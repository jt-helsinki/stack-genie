package hf

// Fake is an in-memory Client for tests. Set the *Err / result fields to control
// behaviour; the zero value returns empty/nil. Download replays DownloadLines through
// the callback before returning DownloadErr.
type Fake struct {
	Cached      []CachedModel
	CacheErr    error
	DownloadErr error
	// DownloadErrs is a per-repo Download error (overrides DownloadErr when the repo has
	// an entry), for testing a partial multi-pull.
	DownloadErrs   map[string]error
	DownloadLines  []string
	DownloadedRepo string   // the last repo passed to Download
	DownloadedAll  []string // every repo passed to Download, in order
	RemoveErr      error
	RemovedRepo    string   // the last repo passed to CacheRemove
	RemovedAll     []string // every repo passed to CacheRemove, in order
}

func (fake *Fake) Download(repo string, progress func(line string)) error {
	fake.DownloadedRepo = repo
	fake.DownloadedAll = append(fake.DownloadedAll, repo)
	if progress != nil {
		for _, line := range fake.DownloadLines {
			progress(line)
		}
	}
	if fake.DownloadErrs != nil {
		if err, ok := fake.DownloadErrs[repo]; ok {
			return err
		}
	}
	return fake.DownloadErr
}

func (fake *Fake) CacheList() ([]CachedModel, error) {
	return fake.Cached, fake.CacheErr
}

func (fake *Fake) CacheRemove(repo string) error {
	fake.RemovedRepo = repo
	fake.RemovedAll = append(fake.RemovedAll, repo)
	return fake.RemoveErr
}
