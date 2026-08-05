package vllm

import (
	"errors"
	"runtime"
	"slices"
	"testing"
)

// swapPyenv captures the pyenv seams and restores them on cleanup.
func swapPyenv(test *testing.T) {
	test.Helper()
	ensure, ensureVersion, pip := pyenvEnsureFn, pyenvEnsureVersionFn, pyenvPipInstallFn
	test.Cleanup(func() {
		pyenvEnsureFn, pyenvEnsureVersionFn, pyenvPipInstallFn = ensure, ensureVersion, pip
	})
}

// swapMetalGet captures the metalReleaseGet HTTP seam and restores it on cleanup.
func swapMetalGet(test *testing.T) {
	test.Helper()
	original := metalReleaseGet
	test.Cleanup(func() { metalReleaseGet = original })
}

// InstallSpecs: no static spec on darwin (the resolver owns it); plain vllm elsewhere.
func TestInstallSpecs(test *testing.T) {
	if darwin := InstallSpecs("darwin"); len(darwin) != 0 {
		test.Errorf("darwin specs = %v, want none (the MLX/Metal resolver owns darwin)", darwin)
	}
	for _, goos := range []string{"linux", "freebsd", ""} {
		if got := InstallSpecs(goos); !slices.Equal(got, []string{"vllm"}) {
			test.Errorf("InstallSpecs(%q) = %v, want [vllm]", goos, got)
		}
	}
}

// Explicit --spec specs are installed VERBATIM: Ensure (newest) then PipInstall(spec),
// and the metal resolver is NOT consulted (metalReleaseGet must not be called).
func TestInstallExplicitSpecOverride(test *testing.T) {
	swapPyenv(test)
	swapMetalGet(test)

	var ensured bool
	var installed []string
	pyenvEnsureFn = func() (bool, error) { ensured = true; return true, nil }
	pyenvEnsureVersionFn = func(string) (bool, error) {
		test.Fatal("EnsureVersion must not be called for an explicit --spec override")
		return false, nil
	}
	pyenvPipInstallFn = func(specs ...string) error { installed = append(installed, specs...); return nil }
	metalReleaseGet = func(string) ([]byte, error) {
		test.Fatal("the metal resolver must not run for an explicit --spec override")
		return nil, nil
	}

	if err := Install("some-wheel.whl"); err != nil {
		test.Fatalf("Install(--spec): %v", err)
	}
	if !ensured {
		test.Error("explicit spec must still ensure the venv")
	}
	if !slices.Equal(installed, []string{"some-wheel.whl"}) {
		test.Errorf("installed = %v, want the verbatim spec", installed)
	}
}

// darwin, no specs: resolve the metal wheel (cp312 → "3.12"), pin the venv to that
// version, and install BOTH the core wheel and the metal wheel.
func TestInstallDarwinResolvesMetal(test *testing.T) {
	if runtime.GOOS != "darwin" {
		test.Skip("darwin resolver branch only runs on darwin")
	}
	swapPyenv(test)
	swapMetalGet(test)

	metalReleaseGet = func(string) ([]byte, error) {
		return []byte(`{"assets":[{"name":"vllm_metal-0.3.0.dev1-cp312-cp312-macosx_11_0_arm64.whl","browser_download_url":"https://example/metal.whl"}]}`), nil
	}
	var pinned string
	var installed []string
	pyenvEnsureFn = func() (bool, error) {
		test.Fatal("darwin metal path must use EnsureVersion, not Ensure")
		return false, nil
	}
	pyenvEnsureVersionFn = func(version string) (bool, error) { pinned = version; return true, nil }
	pyenvPipInstallFn = func(specs ...string) error { installed = append(installed, specs...); return nil }

	if err := Install(); err != nil {
		test.Fatalf("Install(darwin): %v", err)
	}
	if pinned != "3.12" {
		test.Errorf("EnsureVersion pinned %q, want 3.12 (from the cp312 wheel)", pinned)
	}
	if len(installed) != 2 || installed[0] != coreWheelCP312 || installed[1] != "https://example/metal.whl" {
		test.Errorf("installed = %v, want [core-wheel, metal-wheel] in order", installed)
	}
}

// darwin, no specs, resolver HTTP failure → Install returns the error (setup swallows it).
func TestInstallDarwinResolverError(test *testing.T) {
	if runtime.GOOS != "darwin" {
		test.Skip("darwin resolver branch only runs on darwin")
	}
	swapPyenv(test)
	swapMetalGet(test)

	metalReleaseGet = func(string) ([]byte, error) { return nil, errors.New("network down") }
	pyenvEnsureVersionFn = func(string) (bool, error) {
		test.Fatal("EnsureVersion must not run when the resolver fails")
		return false, nil
	}
	pyenvPipInstallFn = func(...string) error {
		test.Fatal("PipInstall must not run when the resolver fails")
		return nil
	}

	if err := Install(); err == nil {
		test.Fatal("Install must return the resolver error on darwin")
	}
}

// non-darwin, no specs: Ensure (newest) then PipInstall("vllm").
func TestInstallLinux(test *testing.T) {
	if runtime.GOOS == "darwin" {
		test.Skip("linux branch only runs off darwin")
	}
	swapPyenv(test)

	var ensured bool
	var installed []string
	pyenvEnsureFn = func() (bool, error) { ensured = true; return true, nil }
	pyenvPipInstallFn = func(specs ...string) error { installed = append(installed, specs...); return nil }

	if err := Install(); err != nil {
		test.Fatalf("Install(linux): %v", err)
	}
	if !ensured {
		test.Error("linux path must ensure the venv")
	}
	if !slices.Equal(installed, []string{"vllm"}) {
		test.Errorf("installed = %v, want [vllm]", installed)
	}
}
