package cmd

import "fmt"

// Version overrides the generated build number when set at build time:
//
//	go build -ldflags "-X github.com/eyal-gor/p_71_cerver_cli/cmd.Version=v133"
var Version = ""

// VersionString is the badge shown in the agents board and session view:
// one natural number ("v132"), bumped on every commit by the pre-commit
// hook that regenerates version_gen.go.
func VersionString() string {
	if Version != "" {
		return Version
	}
	return fmt.Sprintf("v%d", buildNumber)
}
