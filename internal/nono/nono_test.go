package nono

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeProber struct {
	paths map[string]string
}

func (prober fakeProber) LookPath(file string) (string, error) {
	if path, ok := prober.paths[file]; ok {
		return path, nil
	}
	return "", errors.New("not found")
}

func TestDetectInstalled(test *testing.T) {
	info := Detect(fakeProber{paths: map[string]string{"nono": "/opt/homebrew/bin/nono"}})
	if !info.Installed {
		test.Fatal("Installed = false, want true")
	}
	if info.Path != "/opt/homebrew/bin/nono" {
		test.Errorf("Path = %q, want /opt/homebrew/bin/nono", info.Path)
	}
}

func TestDetectMissing(test *testing.T) {
	info := Detect(fakeProber{})
	if info.Installed {
		test.Fatal("Installed = true, want false")
	}
	if info.Path != "" {
		test.Errorf("Path = %q, want empty", info.Path)
	}
}

func TestProfilePath(test *testing.T) {
	got := ProfilePath("/Users/jordan/projects/demo")
	want := filepath.Join("/Users/jordan/projects/demo", ".ai-platform", "nono-profile.json")
	if got != want {
		test.Errorf("ProfilePath() = %q, want %q", got, want)
	}
}

func TestWriteProfileRoundTrip(test *testing.T) {
	root := test.TempDir()
	path := ProfilePath(root)
	profile := Profile{
		Meta: Meta{Name: "demo"},
		Filesystem: FilesystemCapabilities{
			Allow: []string{root},
		},
		Network: NetworkCapabilities{
			AllowDomain: []string{"127.0.0.1"},
			CustomCredentials: map[string]CustomCredential{
				"aip-gateway": {
					Upstream:         "http://127.0.0.1:18787",
					CredentialKey:    "aip_gateway_key",
					InjectHeader:     "Authorization",
					CredentialFormat: "Bearer {}",
				},
			},
		},
	}
	if err := WriteProfile(path, profile); err != nil {
		test.Fatalf("WriteProfile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		test.Fatalf("ReadFile: %v", err)
	}
	var decoded Profile
	if err := json.Unmarshal(raw, &decoded); err != nil {
		test.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Meta.Name != "demo" {
		test.Errorf("Meta.Name = %q, want demo", decoded.Meta.Name)
	}
	if len(decoded.Filesystem.Allow) != 1 || decoded.Filesystem.Allow[0] != root {
		test.Errorf("Filesystem.Allow = %v, want [%s]", decoded.Filesystem.Allow, root)
	}
	credential, ok := decoded.Network.CustomCredentials["aip-gateway"]
	if !ok {
		test.Fatal("CustomCredentials missing aip-gateway entry")
	}
	if credential.Upstream != "http://127.0.0.1:18787" {
		test.Errorf("credential.Upstream = %q, want http://127.0.0.1:18787", credential.Upstream)
	}
}

func TestWriteProfileCreatesParentDirs(test *testing.T) {
	root := test.TempDir()
	nested := filepath.Join(root, "a", "b")
	path := ProfilePath(nested)
	if err := WriteProfile(path, Profile{Meta: Meta{Name: "demo"}}); err != nil {
		test.Fatalf("WriteProfile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		test.Fatalf("profile not written: %v", err)
	}
}

func TestWriteProfileLeavesNoTempFiles(test *testing.T) {
	root := test.TempDir()
	path := ProfilePath(root)
	if err := WriteProfile(path, Profile{Meta: Meta{Name: "demo"}}); err != nil {
		test.Fatalf("WriteProfile: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		test.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "nono-profile.json" {
		names := make([]string, len(entries))
		for index, entry := range entries {
			names[index] = entry.Name()
		}
		test.Errorf("directory contents = %v, want exactly [nono-profile.json]", names)
	}
}
