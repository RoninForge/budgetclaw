// Package version exposes build-time version metadata.
//
// Values are injected at release time via -ldflags:
//
//	go build -ldflags "\
//	  -X github.com/RoninForge/budgetclaw/internal/version.version=v0.1.0 \
//	  -X github.com/RoninForge/budgetclaw/internal/version.commit=$(git rev-parse HEAD) \
//	  -X github.com/RoninForge/budgetclaw/internal/version.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// When the binary is built without ldflags (e.g. `go run`, `go install`
// from source, or unit tests), Get falls back to runtime/debug.ReadBuildInfo
// so developers still see useful data.
package version

import (
	"runtime/debug"
)

// These are set by -ldflags at release time. Lowercase so the only way
// to set them is via -ldflags, keeping the public Get() the single
// source of truth for callers.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// Info describes a built binary.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
}

// Get returns build metadata, preferring ldflags-injected values and
// falling back to runtime/debug.ReadBuildInfo for source builds.
func Get() Info {
	info := Info{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
	}

	// Fill unset fields from BuildInfo when available. This makes
	// `go install github.com/RoninForge/budgetclaw/cmd/budgetclaw@latest`
	// produce useful `version` output even without a release build.
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}

	if info.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}

	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "unknown" && s.Value != "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.BuildDate == "unknown" && s.Value != "" {
				info.BuildDate = s.Value
			}
		}
	}
	return info
}
