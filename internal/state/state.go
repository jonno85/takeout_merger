// Package state persists what the merge step has already done, so re-runs
// and future Takeout rounds skip everything previously imported.
//
// Format: append-only JSON-lines journal (merge.state.jsonl). One record per
// line; the file is replayed into memory on open. For a 50k-file library
// this is a few MB — no database needed, and the journal is human-readable
// for debugging ("why was this file skipped?").
package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one journal line. T selects the record kind:
//
//	"file"  a canonical media file written to the library
//	"album" a hardlink of a canonical file into an album folder
//	"skip"  a source file intentionally not imported (duplicate, superseded)
type Record struct {
	T         string `json:"t"`
	Hash      string `json:"hash,omitempty"`
	Canonical string `json:"canonical,omitempty"` // relative to output root
	Src       string `json:"src,omitempty"`       // source path (staging)
	TakenAt   string `json:"taken,omitempty"`     // RFC3339 UTC
	Album     string `json:"album,omitempty"`
	Link      string `json:"link,omitempty"` // relative to output root
	Reason    string `json:"reason,omitempty"`
	At        string `json:"at,omitempty"`
}

// Store is the in-memory view plus the append handle.
type Store struct {
	mu    sync.Mutex
	f     *os.File // nil in read-only/dry-run mode
	dirty int

	// Files maps content hash -> canonical relative path.
	Files map[string]string
	// Albums maps content hash -> album title -> link relative path.
	Albums map[string]map[string]string
	// Skipped maps source path -> reason (informational).
	Skipped map[string]string
}

// Open loads (or creates) the journal at path. When readOnly is true the
// journal is only replayed, never written (dry-run mode).
func Open(path string, readOnly bool) (*Store, error) {
	s := &Store{
		Files:   map[string]string{},
		Albums:  map[string]map[string]string{},
		Skipped: map[string]string{},
	}

	if b, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(b)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		line := 0
		for sc.Scan() {
			line++
			var r Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				b.Close()
				return nil, fmt.Errorf("%s:%d: corrupt journal line: %w", path, line, err)
			}
			s.apply(r)
		}
		if err := sc.Err(); err != nil {
			b.Close()
			return nil, err
		}
		b.Close()
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		s.f = f
	}
	return s, nil
}

func (s *Store) apply(r Record) {
	switch r.T {
	case "file":
		s.Files[r.Hash] = r.Canonical
	case "album":
		m := s.Albums[r.Hash]
		if m == nil {
			m = map[string]string{}
			s.Albums[r.Hash] = m
		}
		m[r.Album] = r.Link
	case "skip":
		s.Skipped[r.Src] = r.Reason
	}
}

func (s *Store) append(r Record) error {
	r.At = time.Now().UTC().Format(time.RFC3339)
	s.apply(r)
	if s.f == nil {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	// Batch fsyncs: every 100 records is a good durability/throughput
	// balance on NAS disks; a crash loses at most the last batch, which the
	// next run simply redoes (records are written after the FS operations).
	s.dirty++
	if s.dirty >= 100 {
		s.dirty = 0
		return s.f.Sync()
	}
	return nil
}

// AddFile records a canonical file. Safe for concurrent use.
func (s *Store) AddFile(hash, canonical, src string, takenAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	taken := ""
	if !takenAt.IsZero() {
		taken = takenAt.UTC().Format(time.RFC3339)
	}
	return s.append(Record{T: "file", Hash: hash, Canonical: canonical, Src: src, TakenAt: taken})
}

// AddAlbum records an album hardlink. Safe for concurrent use.
func (s *Store) AddAlbum(hash, album, link string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(Record{T: "album", Hash: hash, Album: album, Link: link})
}

// AddSkip records an intentionally skipped source file. Safe for concurrent use.
func (s *Store) AddSkip(src, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(Record{T: "skip", Src: src, Reason: reason})
}

// CanonicalFor returns the canonical path for a content hash, if known.
func (s *Store) CanonicalFor(hash string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.Files[hash]
	return p, ok
}

// HasAlbumLink reports whether hash is already linked into album.
func (s *Store) HasAlbumLink(hash, album string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Albums[hash][album]
	return ok
}

// Close flushes and closes the journal.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	if err := s.f.Sync(); err != nil {
		s.f.Close()
		return err
	}
	return s.f.Close()
}
