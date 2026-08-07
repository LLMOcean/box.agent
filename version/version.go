// Package version holds box-agent's build version, set at compile time via
// -ldflags (see deploy/install.sh's release build step) so a running binary
// can report which tagged release it was actually built from - e.g. to tell
// a stale download apart from the one a GitHub Release actually publishes
// (git tags and GitHub Releases are two different things; see main.go's
// -version flag).
package version

// Version defaults to "dev" for a plain `go build`. Release builds set it
// via `-ldflags "-X github.com/LLMOcean/box.agent/version.Version=vX.Y.Z"`.
var Version = "dev"
