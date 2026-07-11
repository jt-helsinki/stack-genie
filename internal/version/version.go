// Package version holds the build version of the ai binary.
package version

// Version is the semantic version of the build. It is "0.0.0-dev" for local
// builds and overridden at release time via:
//
//	-ldflags "-X github.com/jt-helsinki/stack-genie/internal/version.Version=<v>"
var Version = "0.0.0-dev"
