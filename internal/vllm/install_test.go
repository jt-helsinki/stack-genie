package vllm

import (
	"slices"
	"testing"
)

// InstallSpecs splits by GOOS: the Metal plugin on darwin, the plain vllm package
// elsewhere (Linux/CUDA).
func TestInstallSpecs(test *testing.T) {
	darwin := InstallSpecs("darwin")
	if len(darwin) != 1 || darwin[0] != "git+https://github.com/vllm-project/vllm-metal.git" {
		test.Errorf("darwin specs = %v, want the vLLM-Metal plugin", darwin)
	}
	for _, goos := range []string{"linux", "freebsd", ""} {
		if got := InstallSpecs(goos); !slices.Equal(got, []string{"vllm"}) {
			test.Errorf("InstallSpecs(%q) = %v, want [vllm]", goos, got)
		}
	}
}
