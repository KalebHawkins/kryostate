// Package version says which build of kryostate is running.
//
// A release build sets the three variables with the linker:
//
//	go build -ldflags "-X github.com/KalebHawkins/kryostate/internal/version.Version=v1.2.3 \
//	  -X github.com/KalebHawkins/kryostate/internal/version.Commit=abc1234 \
//	  -X github.com/KalebHawkins/kryostate/internal/version.Date=2026-11-12T00:00:00Z" ./cmd/kryostate
//
// A plain "go build" or "go install" leaves them empty, and they are filled
// from the build information the Go toolchain embeds: the module version for
// "go install ...@v1.2.3", and the VCS revision and commit time for a build
// inside a checkout.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Set with -ldflags -X at release time.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// Info is one build's identity.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go"`
	Platform  string `json:"platform"`
	Modified  bool   `json:"modified,omitempty"`
}

var (
	once sync.Once
	info Info
)

// Get returns the running build's identity, falling back to the embedded
// build information for anything the linker did not set.
func Get() Info {
	once.Do(func() {
		bi, ok := debug.ReadBuildInfo()
		info = resolve(Version, Commit, Date, bi, ok)
	})
	return info
}

func resolve(v, c, d string, bi *debug.BuildInfo, ok bool) Info {
	in := Info{Version: v, Commit: c, Date: d, GoVersion: runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if ok && bi != nil {
		if in.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			in.Version = bi.Main.Version
		}
		// The VCS stamp describes the checkout, so it is only trusted when
		// the linker did not say otherwise: a release build names its own
		// commit, and "modified" would be about a tree it was not built from.
		fromVCS := c == ""
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if in.Commit == "" {
					in.Commit = s.Value
				}
			case "vcs.time":
				if in.Date == "" {
					in.Date = s.Value
				}
			case "vcs.modified":
				in.Modified = fromVCS && s.Value == "true"
			}
		}
		if bi.GoVersion != "" {
			in.GoVersion = bi.GoVersion
		}
	}
	if in.Version == "" {
		in.Version = "dev"
	}
	if len(in.Commit) > 12 {
		in.Commit = in.Commit[:12]
	}
	if in.Commit == "" {
		in.Commit = "unknown"
	}
	if in.Date == "" {
		in.Date = "unknown"
	}
	return in
}

// Short is the version alone, with "+dirty" when built from a checkout with
// uncommitted changes, for the foot of a page.
func Short() string {
	in := Get()
	// Go 1.24 and later already stamp a pseudo-version such as
	// v0.0.0-20261112184625-cc4e36f3455a+dirty on a build from a checkout.
	if in.Modified && !strings.HasSuffix(in.Version, "+dirty") {
		return in.Version + "+dirty"
	}
	return in.Version
}

// String is what "kryostate version" prints.
func (in Info) String() string {
	dirty := ""
	if in.Modified {
		dirty = " (modified)"
	}
	return fmt.Sprintf("kryostate %s\n  commit:   %s%s\n  built:    %s\n  go:       %s\n  platform: %s\n",
		in.Version, in.Commit, dirty, in.Date, in.GoVersion, in.Platform)
}
