# Code Review Consistency Fixes Plan

1. Fix `internal/hostsfile/hostsfile.go:1` so the project builds.
2. Decide the real service image pins and update either specs or `internal/services`.
3. Replace user-facing `ai workspace start` remediation strings with `ai start`.
4. Clean stale hidden-alias comments.
5. Clarify logs scope wording.
6. Decide whether the fake Microsandbox version pin should remain in `versions.yaml`.

After fixes, run:

```zsh
cd /Users/jordan/Documents/workspace/ideal-robot
go test ./...
make fmt-check vet test build
```

