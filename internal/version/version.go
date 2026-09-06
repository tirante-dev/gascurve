// Package version holds the build version, injected at link time with
// -ldflags "-X github.com/tirante-dev/gascurve/internal/version.Version=v1.2.3".
package version

// Version is the semantic version of the binary. "dev" when not set at build time.
var Version = "dev"

// UserAgent returns the User-Agent string sent with every RPC request.
func UserAgent() string {
	return "gascurve/" + Version
}
