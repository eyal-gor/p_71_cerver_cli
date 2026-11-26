package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveNameStaysInsideAndUnique(t *testing.T) {
	dir := t.TempDir()
	if p := saveName(dir, "../../etc/passwd", 1); filepath.Dir(p) != dir {
		t.Fatalf("escaped: %s", p)
	}
	if p := saveName(dir, "", 3); filepath.Base(p) != "file-3" {
		t.Fatalf("empty name: %s", p)
	}
	os.WriteFile(filepath.Join(dir, "IMG_1.jpg"), nil, 0o644)
	if p := saveName(dir, "IMG_1.jpg", 1); filepath.Base(p) != "IMG_1-2.jpg" {
		t.Fatalf("not unique: %s", p)
	}
}
