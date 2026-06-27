// Package version exposes build identity injected at link time via -ldflags.
// In a plain `go build` (dev) the values stay at their safe zero defaults.
package version

import "runtime"

// These are overridden at build time, e.g.:
//
//	go build -ldflags "-X github.com/calystral-io/studio/internal/version.Version=0.1.0 \
//	  -X github.com/calystral-io/studio/internal/version.Commit=abc1234 \
//	  -X github.com/calystral-io/studio/internal/version.BuildTime=2026-06-27T15:00:00Z"
var (
	// Version is the semantic version of the build. Defaults to a dev marker.
	Version = "0.0.0-dev"
	// Commit is the short git SHA of the build. Empty in dev.
	Commit = ""
	// BuildTime is the RFC3339 UTC build timestamp. Empty in dev.
	BuildTime = ""
)

// Service is the stable service identifier surfaced on /api/v1/version.
const Service = "studio"

// Info is the structured build identity returned by the version endpoint.
type Info struct {
	Service   string `json:"service"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Go        string `json:"go"`
	BuildTime string `json:"build_time"`
}

// Current returns the build identity for this binary.
func Current() Info {
	return Info{
		Service:   Service,
		Version:   Version,
		Commit:    Commit,
		Go:        runtime.Version(),
		BuildTime: BuildTime,
	}
}
