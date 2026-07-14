// Package version holds the tncd version string.
package version

// Version is overridable at build time via
// -ldflags "-X github.com/ben-kuhn/tncd/v2/internal/version.Version=..."
var Version = "2.0.0-dev"
