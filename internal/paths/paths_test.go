package paths_test

import (
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

func TestHomeFollowsHOME(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := paths.Home()
	if err != nil {
		t.Fatalf("Home returned error: %v", err)
	}
	if got != home {
		t.Errorf("Home() = %q, want %q", got, home)
	}
}

func TestPathHelpersRootedUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	platformDir := filepath.Join(home, ".ai-platform")

	testCases := []struct {
		name   string
		helper func() (string, error)
		want   string
	}{
		{
			name:   "PlatformDir",
			helper: paths.PlatformDir,
			want:   platformDir,
		},
		{
			name:   "ConfigDir",
			helper: paths.ConfigDir,
			want:   filepath.Join(platformDir, "config"),
		},
		{
			name:   "LogsDir",
			helper: paths.LogsDir,
			want:   filepath.Join(platformDir, "logs"),
		},
		{
			name:   "VolumesDir",
			helper: paths.VolumesDir,
			want:   filepath.Join(platformDir, "volumes"),
		},
		{
			name:   "OverlaysDir",
			helper: paths.OverlaysDir,
			want:   filepath.Join(platformDir, "overlays"),
		},
		{
			name:   "ProjectsDir",
			helper: paths.ProjectsDir,
			want:   filepath.Join(home, "projects"),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := testCase.helper()
			if err != nil {
				t.Fatalf("%s returned error: %v", testCase.name, err)
			}
			if got != testCase.want {
				t.Errorf("%s() = %q, want %q", testCase.name, got, testCase.want)
			}
		})
	}
}

func TestPathHelpersTrackChangedHOME(t *testing.T) {
	// Each helper must re-resolve from HOME on every call rather than caching,
	// so the acceptance harness can repoint HOME at a throwaway dir.
	firstHome := t.TempDir()
	t.Setenv("HOME", firstHome)

	firstPlatform, err := paths.PlatformDir()
	if err != nil {
		t.Fatalf("PlatformDir returned error: %v", err)
	}
	if want := filepath.Join(firstHome, ".ai-platform"); firstPlatform != want {
		t.Fatalf("PlatformDir() = %q, want %q", firstPlatform, want)
	}

	secondHome := t.TempDir()
	t.Setenv("HOME", secondHome)

	secondPlatform, err := paths.PlatformDir()
	if err != nil {
		t.Fatalf("PlatformDir returned error: %v", err)
	}
	if want := filepath.Join(secondHome, ".ai-platform"); secondPlatform != want {
		t.Errorf("PlatformDir() did not track changed HOME: = %q, want %q", secondPlatform, want)
	}
}

func TestHelpersPropagateHomeError(t *testing.T) {
	// On macOS/Linux os.UserHomeDir reports an error when HOME is empty; the
	// helpers must surface that rather than returning a bogus relative path.
	t.Setenv("HOME", "")

	testCases := []struct {
		name   string
		helper func() (string, error)
	}{
		{"Home", paths.Home},
		{"PlatformDir", paths.PlatformDir},
		{"ConfigDir", paths.ConfigDir},
		{"LogsDir", paths.LogsDir},
		{"VolumesDir", paths.VolumesDir},
		{"OverlaysDir", paths.OverlaysDir},
		{"ProjectsDir", paths.ProjectsDir},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := testCase.helper()
			if err == nil {
				t.Fatalf("%s with empty HOME = %q, want error", testCase.name, got)
			}
			if got != "" {
				t.Errorf("%s returned %q alongside error, want empty string", testCase.name, got)
			}
		})
	}
}

func TestConfigDirNestedUnderPlatformDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	platformDir, err := paths.PlatformDir()
	if err != nil {
		t.Fatalf("PlatformDir returned error: %v", err)
	}
	configDir, err := paths.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir returned error: %v", err)
	}
	if filepath.Dir(configDir) != platformDir {
		t.Errorf("ConfigDir %q is not nested directly under PlatformDir %q", configDir, platformDir)
	}
}
