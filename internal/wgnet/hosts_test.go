package wgnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostName(t *testing.T) {
	for in, want := range map[string]string{
		"Eyal's MacBook Pro": "eyals-macbook-pro",
		"mac-mini":           "mac-mini",
		"  e2b_cloud  ":      "e2b-cloud",
		"!!!":                "",
	} {
		if got := HostName(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestHostsBlockKeepsTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	orig := "127.0.0.1\tlocalhost\n::1\tlocalhost\n"
	os.WriteFile(path, []byte(orig), 0644)

	b := HostsBlock(map[string]string{"100.96.0.10": "Mac Mini", "100.96.0.2": "MacBook", "100.96.0.3": "macbook"})
	if err := WriteHosts(path, b); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if !strings.HasPrefix(s, orig) {
		t.Fatalf("lost the original lines:\n%s", s)
	}
	if !strings.Contains(s, "100.96.0.2\tmacbook macbook.cerver\n") || strings.Contains(s, "100.96.0.3") {
		t.Fatalf("duplicate name should go to the lowest address only:\n%s", s)
	}
	if !strings.Contains(s, "100.96.0.10\tmac-mini mac-mini.cerver\n") {
		t.Fatalf("missing mac-mini:\n%s", s)
	}

	// Rewrite replaces, never stacks; removing restores the original exactly.
	WriteHosts(path, HostsBlock(map[string]string{"100.96.0.2": "macbook"}))
	got, _ = os.ReadFile(path)
	if strings.Count(string(got), hostsBegin) != 1 || strings.Contains(string(got), "mac-mini") {
		t.Fatalf("block stacked or stale:\n%s", got)
	}
	WriteHosts(path, "")
	got, _ = os.ReadFile(path)
	if string(got) != orig {
		t.Fatalf("not restored:\n%q", got)
	}
}
