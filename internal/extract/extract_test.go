package extract

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTGZ creates a synthetic takeout-like archive in dir and returns its path.
func buildTGZ(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for p, content := range files {
		hdr := &tar.Header{
			Name:    p,
			Mode:    0o644,
			Size:    int64(len(content)),
			ModTime: time.Date(2019, 9, 20, 15, 32, 22, 0, time.UTC),
			Format:  tar.FormatPAX, // long/UTF-8 names go through PAX records
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractPAXLongNamesAndResume(t *testing.T) {
	archives := t.TempDir()
	staging := t.TempDir()

	// >100-char path forces PAX extended headers — the exact case DSM's
	// busybox tar corrupts into PaxHeaders.X junk.
	longName := "Takeout/Google Photos/Photos from 2019/" +
		strings.Repeat("fotografia-con-nome-molto-lungo-", 3) + "ù.jpg"

	files := map[string]string{
		"Takeout/Google Photos/Photos from 2019/IMG_1.jpg":      "jpegdata",
		"Takeout/Google Photos/Photos from 2019/IMG_1.jpg.json": `{"title":"IMG_1.jpg"}`,
		longName: "jpegdata2",
	}
	buildTGZ(t, archives, "takeout-001.tgz", files)

	opts := Options{ArchivesDir: archives, StagingDir: staging}
	res, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Archives != 1 || res.Files != 3 {
		t.Fatalf("res = %+v", res)
	}

	// long UTF-8 name must come out intact, no PaxHeaders junk anywhere
	if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(longName))); err != nil {
		t.Errorf("long PAX name not extracted correctly: %v", err)
	}
	filepath.WalkDir(staging, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), "PaxHeaders") {
			t.Errorf("PaxHeaders junk in staging: %s", p)
		}
		return nil
	})

	// mtime restored from the tar header
	info, err := os.Stat(filepath.Join(staging, "Takeout/Google Photos/Photos from 2019/IMG_1.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().UTC().Equal(time.Date(2019, 9, 20, 15, 32, 22, 0, time.UTC)) {
		t.Errorf("mtime not restored: %v", info.ModTime())
	}

	// resume: second run must skip via journal
	res2, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Archives != 0 || res2.SkippedArchives != 1 {
		t.Fatalf("resume res = %+v", res2)
	}
}

func TestUnsafePathsSkipped(t *testing.T) {
	archives := t.TempDir()
	staging := t.TempDir()

	buildTGZ(t, archives, "evil.tgz", map[string]string{
		"../outside.txt":   "nope",
		"ok/inside.txt":    "yes",
		"/abs/path.txt":    "nope",
		"PaxHeaders.1/x":   "junk",
		"a/PaxHeaders.2/y": "junk",
	})

	res, err := Run(Options{ArchivesDir: archives, StagingDir: staging})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Fatalf("expected only the safe file, got %d", res.Files)
	}
	if _, err := os.Stat(filepath.Join(staging, "ok/inside.txt")); err != nil {
		t.Error("safe file missing")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(staging), "outside.txt")); err == nil {
		t.Error("path traversal escaped the staging dir!")
	}
}

func TestDryRun(t *testing.T) {
	archives := t.TempDir()
	staging := t.TempDir()
	buildTGZ(t, archives, "t.tgz", map[string]string{"a/b.jpg": "x"})

	res, err := Run(Options{ArchivesDir: archives, StagingDir: staging, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Fatalf("res = %+v", res)
	}
	entries, _ := os.ReadDir(staging)
	if len(entries) != 0 {
		t.Error("dry-run must not write to staging")
	}
}
