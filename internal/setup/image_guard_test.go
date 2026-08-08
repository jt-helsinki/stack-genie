package setup

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// imageGuardProber answers the argv containerOnCurrentImage issues: `ps` (running
// check), `inspect -f {{.Image}} <name>` (the container's image id) and `image
// inspect -f {{.Id}} <ref>` (the ref's local image id). It also answers the plain
// `image inspect <ref>` that pullImages uses to test presence. Any map miss yields
// an empty id; inspectErr/refErr force the respective inspect to fail.
type imageGuardProber struct {
	running        map[string]bool
	containerImage map[string]string // container name → its running image id
	refImage       map[string]string // image ref → local image id
	inspectErr     bool              // container `inspect` returns an error
	refErr         bool              // `image inspect` returns an error
}

func (prober imageGuardProber) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }
func (prober imageGuardProber) Exists(string) bool                   { return false }
func (prober imageGuardProber) Run(_ string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "ps":
		for _, arg := range args {
			if container, ok := strings.CutPrefix(arg, "name="); ok {
				if prober.running[container] {
					return []byte(container + "\n"), nil
				}
			}
		}
		return nil, nil
	case "inspect": // inspect -f {{.Image}} <name>
		if prober.inspectErr {
			return nil, errors.New("no such object")
		}
		return []byte(prober.containerImage[args[len(args)-1]] + "\n"), nil
	case "image": // image inspect [-f {{.Id}}] <ref>
		if prober.refErr {
			return nil, errors.New("no such image")
		}
		return []byte(prober.refImage[args[len(args)-1]] + "\n"), nil
	}
	return nil, nil
}

// TestContainerOnCurrentImage: the ensure* recreate guard is true ONLY when the
// container is running on the SAME local image id as the ref it would run; a stale
// image id, a stopped container, or either inspect erroring all force a recreate.
func TestContainerOnCurrentImage(test *testing.T) {
	const name = "aip-litellm"
	const ref = "ghcr.io/berriai/litellm:latest"

	// Running on the SAME image id → current (skip the recreate).
	current := imageGuardProber{
		running:        map[string]bool{name: true},
		containerImage: map[string]string{name: "sha256:aaa"},
		refImage:       map[string]string{ref: "sha256:aaa"},
	}
	if !containerOnCurrentImage(current, "docker", name, ref) {
		test.Error("matching image ids must report the container as on the current image")
	}

	// Running on a DIFFERENT (stale) image id → NOT current (must recreate).
	stale := imageGuardProber{
		running:        map[string]bool{name: true},
		containerImage: map[string]string{name: "sha256:OLD"},
		refImage:       map[string]string{ref: "sha256:NEW"},
	}
	if containerOnCurrentImage(stale, "docker", name, ref) {
		test.Error("a stale image id must NOT report current (must recreate to apply the pulled image)")
	}

	// Not running → false (nothing to skip).
	stopped := imageGuardProber{
		containerImage: map[string]string{name: "sha256:aaa"},
		refImage:       map[string]string{ref: "sha256:aaa"},
	}
	if containerOnCurrentImage(stopped, "docker", name, ref) {
		test.Error("a stopped container is not on the current image")
	}

	// Container inspect errors → treated as NOT current.
	inspectErr := imageGuardProber{
		running:    map[string]bool{name: true},
		inspectErr: true,
		refImage:   map[string]string{ref: "sha256:aaa"},
	}
	if containerOnCurrentImage(inspectErr, "docker", name, ref) {
		test.Error("a container-inspect error must be treated as not current")
	}

	// Ref image inspect errors → treated as NOT current.
	refErr := imageGuardProber{
		running:        map[string]bool{name: true},
		containerImage: map[string]string{name: "sha256:aaa"},
		refErr:         true,
	}
	if containerOnCurrentImage(refErr, "docker", name, ref) {
		test.Error("a ref-image-inspect error must be treated as not current")
	}
}

// TestPullImagesSkipsPresentImages pins the non-force PRE-pull behavior that let a
// moved `latest` tag go stale: every required image reports present (`image inspect`
// returns no error), so PullImages SKIPS them all and never execs a real `docker
// pull`. `ai setup` now calls UpdateImages (force=true) instead, which does NOT skip
// — the live force-pull argv itself is a hardware bring-up seam (real runtime exec).
func TestPullImagesSkipsPresentImages(test *testing.T) {
	services := realServices{prober: imageGuardProber{}} // any `image inspect` → present (no error)
	if err := services.PullImages(nil, nil, io.Discard, nil); err != nil {
		test.Fatalf("PullImages must skip present images without attempting a real pull: %v", err)
	}
}
