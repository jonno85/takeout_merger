package merge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jonathan/takeout-merger/internal/matcher"
)

// buildStaging creates a synthetic extracted Takeout tree.
func buildStaging(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gp := filepath.Join(root, "Takeout", "Google Photos")

	write := func(rel, content string) {
		p := filepath.Join(gp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sidecar := func(title, ts string) string {
		return `{"title":"` + title + `","photoTakenTime":{"timestamp":"` + ts + `"},` +
			`"geoData":{"latitude":45.07,"longitude":7.68,"altitude":239}}`
	}

	// Year folder: IMG_1 + sidecar (taken 2019-09-20)
	write("Photos from 2019/IMG_1.jpg", "JPEGDATA-1")
	write("Photos from 2019/IMG_1.jpg.supplemental-metadata.json", sidecar("IMG_1.jpg", "1568991142"))

	// Album folder with SAME bytes for IMG_1 (Takeout duplicates album content)
	write("Vacanza/metadata.json", `{"title":"Vacanza Toscana"}`)
	write("Vacanza/IMG_1.jpg", "JPEGDATA-1")
	write("Vacanza/IMG_1.jpg.supplemental-metadata.json", sidecar("IMG_1.jpg", "1568991142"))

	// Edited pair: IMG_2 original + Italian "-modificato" edit
	write("Photos from 2019/IMG_2.jpg", "ORIGINAL-2")
	write("Photos from 2019/IMG_2-modificato.jpg", "EDITED-2")
	write("Photos from 2019/IMG_2.jpg.supplemental-metadata.json", sidecar("IMG_2.jpg", "1568991142"))

	// Live photo: HEIC with sidecar + MP4 without
	write("Photos from 2019/IMG_3.HEIC", "HEICDATA-3")
	write("Photos from 2019/IMG_3.MP4", "MP4DATA-3")
	write("Photos from 2019/IMG_3.HEIC.supplemental-metadata.json", sidecar("IMG_3.HEIC", "1568991142"))

	// Junk that must be ignored
	write("PaxHeaders.123/junk", "x")

	return root
}

func opts(t *testing.T, staging string) Options {
	t.Helper()
	out := t.TempDir()
	return Options{
		Input:       staging,
		Output:      out,
		StatePath:   filepath.Join(out, ".state", "merge.state.jsonl"),
		Workers:     2,
		ExiftoolBin: "none", // unit tests never require exiftool
		Matcher:     matcher.DefaultConfig(),
	}
}

func TestMergeEndToEnd(t *testing.T) {
	staging := buildStaging(t)
	o := opts(t, staging)

	st, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}

	// IMG_1: one canonical copy...
	canon := filepath.Join(o.Output, "library/2019/09/IMG_1.jpg")
	if _, err := os.Stat(canon); err != nil {
		t.Fatalf("canonical IMG_1 missing: %v", err)
	}
	// ...its album copy deduped into a hardlink
	link := filepath.Join(o.Output, "albums/Vacanza Toscana/IMG_1.jpg")
	ci, err1 := os.Stat(canon)
	li, err2 := os.Stat(link)
	if err1 != nil || err2 != nil {
		t.Fatalf("album link missing: %v %v", err1, err2)
	}
	if !os.SameFile(ci, li) {
		t.Error("album entry is not a hardlink of the canonical file")
	}
	if st.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1 (album copy of IMG_1)", st.Duplicates)
	}

	// IMG_2: edited bytes under the original's name; original skipped
	b, err := os.ReadFile(filepath.Join(o.Output, "library/2019/09/IMG_2.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "EDITED-2" {
		t.Errorf("IMG_2 content = %q, want edited bytes", b)
	}
	if st.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1", st.Superseded)
	}
	if _, err := os.Stat(filepath.Join(o.Output, "library/2019/09/IMG_2-modificato.jpg")); err == nil {
		t.Error("edited file must not appear under its own name")
	}

	// IMG_3.MP4 borrowed the HEIC's sidecar -> dated folder, not undated
	if _, err := os.Stat(filepath.Join(o.Output, "library/2019/09/IMG_3.MP4")); err != nil {
		t.Errorf("live photo video not dated via borrowed sidecar: %v", err)
	}

	// mtime set from sidecar taken time
	if info, err := os.Stat(canon); err == nil {
		if info.ModTime().UTC().Format("2006-01-02") != "2019-09-20" {
			t.Errorf("mtime = %v, want 2019-09-20", info.ModTime().UTC())
		}
	}

	// nothing from PaxHeaders leaked
	filepath.WalkDir(o.Output, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "junk" {
			t.Errorf("PaxHeaders content leaked into output: %s", p)
		}
		return nil
	})

	if st.NewFiles != 4 { // IMG_1, IMG_2(edited), IMG_3.HEIC, IMG_3.MP4
		t.Errorf("NewFiles = %d, want 4", st.NewFiles)
	}

	// ---- idempotent re-run: everything is already known ----
	st2, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if st2.NewFiles != 0 {
		t.Errorf("re-run NewFiles = %d, want 0", st2.NewFiles)
	}
	if st2.AlbumLinks != 0 {
		t.Errorf("re-run AlbumLinks = %d, want 0", st2.AlbumLinks)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	staging := buildStaging(t)
	o := opts(t, staging)
	o.DryRun = true

	st, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if st.NewFiles == 0 {
		t.Error("dry-run should still report planned new files")
	}
	entries, _ := os.ReadDir(o.Output)
	if len(entries) != 0 {
		t.Errorf("dry-run wrote into output: %v", entries)
	}
}

func TestKeepOriginals(t *testing.T) {
	staging := buildStaging(t)
	o := opts(t, staging)
	o.KeepOriginals = true

	if _, err := Run(o); err != nil {
		t.Fatal(err)
	}
	// Both original and edited exist (edited under its own name).
	if _, err := os.Stat(filepath.Join(o.Output, "library/2019/09/IMG_2.jpg")); err != nil {
		t.Error("original missing with --keep-originals")
	}
	if _, err := os.Stat(filepath.Join(o.Output, "library/2019/09/IMG_2-modificato.jpg")); err != nil {
		t.Error("edited variant missing with --keep-originals")
	}
}

func TestNameCollisionDifferentContent(t *testing.T) {
	root := t.TempDir()
	gp := filepath.Join(root, "Takeout", "Google Photos")
	sidecar := `{"photoTakenTime":{"timestamp":"1568991142"}}`
	for _, d := range []string{"Photos from 2019", "Photos from 2019 2"} {
		if err := os.MkdirAll(filepath.Join(gp, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(gp, "Photos from 2019/IMG.jpg"), []byte("AAA"), 0o644)
	os.WriteFile(filepath.Join(gp, "Photos from 2019/IMG.jpg.json"), []byte(sidecar), 0o644)
	os.WriteFile(filepath.Join(gp, "Photos from 2019 2/IMG.jpg"), []byte("BBB"), 0o644)
	os.WriteFile(filepath.Join(gp, "Photos from 2019 2/IMG.jpg.json"), []byte(sidecar), 0o644)

	o := opts(t, root)
	st, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if st.NewFiles != 2 {
		t.Fatalf("NewFiles = %d, want 2 (different content, same name)", st.NewFiles)
	}
	entries, _ := os.ReadDir(filepath.Join(o.Output, "library/2019/09"))
	if len(entries) != 2 {
		t.Fatalf("expected 2 files in date dir, got %d", len(entries))
	}
}
