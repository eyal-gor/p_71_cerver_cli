// Package version holds the release this binary was built as. Releases set it
// with -ldflags "-X github.com/eyal-gor/p_71_cerver_cli/internal/version.Version=1.0.N"
// (see .github/workflows/release.yml); a local build says "dev".
package version

import (
	"strconv"
	"strings"
)

var Version = "dev"

// Newer reports whether release a is newer than b ("1.0.42" > "1.0.9").
// "dev" is never older than anything: a local build doesn't get replaced.
func Newer(a, b string) bool {
	if a == "dev" || b == "dev" || a == "" || b == "" {
		return false
	}
	pa, pb := parts(a), parts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func parts(v string) []int {
	var out []int
	for _, s := range strings.Split(strings.TrimPrefix(v, "v"), ".") {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}
