package cmd

import "testing"

func TestAccessLists(t *testing.T) {
	r := accessRules{Mode: "as_you"}
	r, _, _ = applyAccess(r, "allow", []string{"~/Code"})
	r, _, _ = applyAccess(r, "ask", []string{"~/Code"}) // moves, never in two lists
	if len(r.Allowed) != 0 || len(r.Ask) != 1 {
		t.Fatalf("%+v", r)
	}
	r, _, _ = applyAccess(r, "unlist", []string{"~/Code"})
	if len(r.Ask) != 0 {
		t.Fatal("unlist")
	}
	if _, _, err := applyAccess(r, "mode", []string{"root"}); err == nil {
		t.Fatal("bad mode accepted")
	}
	if got := tildePath("/Users/a", "/Users/a/Code/x"); got != "~/Code/x" {
		t.Fatal(got)
	}
}
