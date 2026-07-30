package cmd

import (
	"fmt"
	"runtime/debug"
)

// Version overrides the generated build number when set at build time:
//
//	go build -ldflags "-X github.com/eyal-gor/p_71_cerver_cli/cmd.Version=v133"
var Version = ""

// VersionString is the badge shown in the agents board and session view:
// one natural number ("v132"), bumped on every commit by the pre-commit
// hook that regenerates version_gen.go. A trailing * means the binary
// was built from uncommitted changes.
func VersionString() string {
	if Version != "" {
		return Version
	}
	v := fmt.Sprintf("v%d", buildNumber)
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.modified" && s.Value == "true" {
				return v + "*"
			}
		}
	}
	return v
}
