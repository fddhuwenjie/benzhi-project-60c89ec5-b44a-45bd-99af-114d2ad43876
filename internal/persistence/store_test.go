package persistence

import (
	"path/filepath"
	"restoration-quality/internal/domain"
	"testing"
	"time"
)

func TestSnapshotRecovery(t *testing.T) {
	dir := t.TempDir()
	s, e := New(filepath.Join(dir, "data"))
	if e != nil {
		t.Fatal(e)
	}
	p, _ := domain.NewProject("p", "a", "t", "c", time.Now())
	if e = s.Create(p); e != nil {
		t.Fatal(e)
	}
	s2, e := New(filepath.Join(dir, "data"))
	if e != nil {
		t.Fatal(e)
	}
	got, e := s2.Get("p")
	if e != nil || got.Title != "t" {
		t.Fatalf("recovery failed: %v", e)
	}
}
