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
	// Auth-related fields.
	LoginErr    error  // returned by Login
	LoginToken  string // the last token passed to Login
	WhoamiUser  string // returned by Whoami on success
	WhoamiErr   error  // returned by Whoami (simulates "not logged in")
	LogoutErr   error  // returned by Logout
	LogoutCalls int    // number of times Logout was called
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

func (fake *Fake) Login(token string) error {
	fake.LoginToken = token
	return fake.LoginErr
}

func (fake *Fake) Whoami() (string, error) {
	return fake.WhoamiUser, fake.WhoamiErr
}

func (fake *Fake) Logout() error {
	fake.LogoutCalls++
	return fake.LogoutErr
}
