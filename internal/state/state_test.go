package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merge.state.jsonl")

	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	taken := time.Date(2019, 9, 20, 14, 52, 22, 0, time.UTC)
	if err := s.AddFile("h1", "library/2019/09/a.jpg", "/staging/a.jpg", taken); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAlbum("h1", "Vacanza Toscana", "albums/Vacanza Toscana/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSkip("/staging/dup.jpg", "duplicate"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// reload
	s2, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := s2.CanonicalFor("h1"); !ok || p != "library/2019/09/a.jpg" {
		t.Errorf("CanonicalFor = (%q,%v)", p, ok)
	}
	if !s2.HasAlbumLink("h1", "Vacanza Toscana") {
		t.Error("album link lost on reload")
	}
	if s2.Skipped["/staging/dup.jpg"] != "duplicate" {
		t.Error("skip lost on reload")
	}
}

func TestReadOnlyNeverWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AddFile("h", "c", "src", time.Time{})
	_ = s.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("read-only store must not create the journal file")
	}
}

func TestConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = s.AddFile(string(rune('a'+n%26))+"h", "c", "s", time.Time{})
			_ = s.AddSkip("src", "r")
		}(i)
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, true); err != nil {
		t.Fatalf("journal corrupted by concurrent writes: %v", err)
	}
}

func TestCorruptJournalDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("{\"t\":\"file\",\"hash\":\"h\"}\nnot-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, true); err == nil {
		t.Fatal("expected error on corrupt journal")
	}
}
