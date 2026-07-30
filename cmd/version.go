package cmd

import (
	"runtime/debug"
	"strings"
)

// baseVersion is the release line shown in the fleet header. Bump on
// release; local builds append the short commit (and * when dirty).
const baseVersion = "0.2.0"

// Version overrides everything when set at build time:
//
//	go build -ldflags "-X github.com/eyal-gor/p_71_cerver_cli/cmd.Version=v1.2.3"
var Version = ""

// VersionString resolves the version to display, best source first:
// ldflags override → module version (go install …@vX.Y.Z) → baseVersion
// plus the vcs revision baked into locally-built binaries.
func VersionString() string {
	if Version != "" {
		return Version
	}
	v := "v" + baseVersion
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	// Real tags only — pseudo-versions (v0.0.0-2026…-abc123) read as
	// noise in a header badge; those fall through to base+revision.
	if mv := bi.Main.Version; mv != "" && mv != "(devel)" && !strings.HasPrefix(mv, "v0.0.0-") {
		return mv
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) >= 7 {
		v += "+" + rev[:7]
		if dirty {
			v += "*"
		}
	}
	return v
}
