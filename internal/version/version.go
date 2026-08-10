// Package version holds the build stamp shared by the action and the CLI.
package version

// Version is overwritten at build time via -ldflags. The default is what an
// unstamped `go build` or `go run` produces.
var Version = "dev"
