package hostpath

import "testing"

func TestToWSLMount(test *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{`C:\Users\me\app`, "/mnt/c/Users/me/app", true},
		{`D:/work/project`, "/mnt/d/work/project", true},
		{`c:\`, "/mnt/c", true},
		{"/home/me/app", "/home/me/app", false},     // already POSIX
		{`\\server\share`, `\\server\share`, false}, // UNC, not a drive path
		{"relative/path", "relative/path", false},
	}
	for _, testCase := range cases {
		got, ok := ToWSLMount(testCase.in)
		if got != testCase.want || ok != testCase.wantOK {
			test.Errorf("ToWSLMount(%q) = (%q,%v), want (%q,%v)", testCase.in, got, ok, testCase.want, testCase.wantOK)
		}
	}
}

func TestWorkspaceMount(test *testing.T) {
	// On Windows the project path is translated to the WSL2 mount view.
	if got := WorkspaceMount("windows", `C:\Users\me\app`); got != "/mnt/c/Users/me/app" {
		test.Errorf("windows mount = %q", got)
	}
	// On macOS/Linux the POSIX host path is used unchanged.
	for _, goos := range []string{"darwin", "linux"} {
		if got := WorkspaceMount(goos, "/Users/me/app"); got != "/Users/me/app" {
			test.Errorf("%s mount = %q, want unchanged", goos, got)
		}
	}
}
