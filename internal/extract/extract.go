// Package extract implements step 1 of the pipeline: turning Google Takeout
// .tgz archives into a clean staging tree.
//
// Why not just `tar -xzf` on the NAS? Two reasons:
//   - DSM's busybox tar mishandles PAX extended headers, producing junk
//     "PaxHeaders.X" directories and truncating long/UTF-8 filenames.
//     Go's archive/tar applies PAX headers natively, so names come out intact.
//   - We keep a journal so a multi-hour extraction of 120 GB survives
//     interruption: already-completed archives are skipped on re-run.
package extract

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options configures an extraction run.
type Options struct {
	ArchivesDir    string
	StagingDir     string
	StateDir       string // journal location; default <staging>/.merger
	DryRun         bool
	DeleteArchives bool
	ProgressEvery  int
}

// Result summarizes an extraction run.
type Result struct {
	Archives        int // archives processed in this run
	SkippedArchives int // archives skipped because the journal marked them done
	Files           int
	Bytes           int64
}

// Run extracts every *.tgz / *.tar.gz found in Options.ArchivesDir into
// Options.StagingDir, resuming from the journal when present.
func Run(opts Options) (Result, error) {
	var res Result

	archives, err := findArchives(opts.ArchivesDir)
	if err != nil {
		return res, err
	}
	if len(archives) == 0 {
		return res, fmt.Errorf("no .tgz or .tar.gz archives found in %s", opts.ArchivesDir)
	}

	if opts.StateDir == "" {
		opts.StateDir = filepath.Join(opts.StagingDir, ".merger")
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 500
	}

	j, err := openJournal(opts.StateDir, opts.DryRun)
	if err != nil {
		return res, err
	}
	defer j.Close()

	for _, arc := range archives {
		name := filepath.Base(arc)
		if j.Done(name) {
			log.Printf("skip %s (journal: already extracted)", name)
			res.SkippedArchives++
			continue
		}

		files, bytes, err := extractArchive(arc, opts)
		if err != nil {
			return res, fmt.Errorf("%s: %w", name, err)
		}
		res.Archives++
		res.Files += files
		res.Bytes += bytes

		if !opts.DryRun {
			if err := j.MarkDone(name, files, bytes); err != nil {
				return res, err
			}
			if opts.DeleteArchives {
				if err := os.Remove(arc); err != nil {
					log.Printf("warning: could not delete %s: %v", name, err)
				} else {
					log.Printf("deleted %s", name)
				}
			}
		}
	}
	return res, nil
}

func findArchives(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := strings.ToLower(e.Name())
		if strings.HasSuffix(n, ".tgz") || strings.HasSuffix(n, ".tar.gz") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// extractArchive streams one .tgz into the staging directory.
func extractArchive(path string, opts Options) (files int, written int64, err error) {
	name := filepath.Base(path)
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	log.Printf("extracting %s (%s compressed)%s", name, HumanBytes(info.Size()), dryTag(opts.DryRun))
	start := time.Now()

	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return 0, 0, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, written, fmt.Errorf("tar: %w", err)
		}

		// archive/tar consumes PAX/GNU extended headers transparently, but be
		// defensive against archives whose *literal* entries carry PaxHeaders
		// paths (seen when archives were re-packed by a broken extractor).
		if hdr.Typeflag != tar.TypeReg || isPaxJunk(hdr.Name) {
			continue
		}

		rel, ok := safeRelPath(hdr.Name)
		if !ok {
			log.Printf("warning: skipping unsafe path in archive: %q", hdr.Name)
			continue
		}
		dst := filepath.Join(opts.StagingDir, rel)

		if opts.DryRun {
			files++
			written += hdr.Size
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return files, written, err
		}
		n, err := writeFile(dst, tr, hdr)
		if err != nil {
			return files, written, fmt.Errorf("write %s: %w", rel, err)
		}
		if n != hdr.Size {
			return files, written, fmt.Errorf("size mismatch for %s: wrote %d, header says %d", rel, n, hdr.Size)
		}

		files++
		written += n
		if files%opts.ProgressEvery == 0 {
			log.Printf("  %s: %d files, %s", name, files, HumanBytes(written))
		}
	}

	log.Printf("done %s: %d files, %s in %s", name, files, HumanBytes(written), time.Since(start).Round(time.Second))
	return files, written, nil
}

func writeFile(dst string, r io.Reader, hdr *tar.Header) (int64, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	// Preserve the archive's modification time; the merge step prefers the
	// JSON sidecar time, but this keeps the staging tree honest meanwhile.
	if !hdr.ModTime.IsZero() {
		_ = os.Chtimes(dst, hdr.ModTime, hdr.ModTime)
	}
	return n, nil
}

// safeRelPath cleans an archive entry name and rejects anything that would
// escape the staging directory (absolute paths, "..", Windows drive letters).
func safeRelPath(name string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, `/`)
	p := filepath.Clean(filepath.FromSlash(name))
	if p == "." || filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return "", false
	}
	if len(p) >= 2 && p[1] == ':' { // C: etc.
		return "", false
	}
	return p, true
}

func isPaxJunk(name string) bool {
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if strings.HasPrefix(part, "PaxHeaders") {
			return true
		}
	}
	return false
}

func dryTag(dry bool) string {
	if dry {
		return " [dry-run]"
	}
	return ""
}

// HumanBytes renders a byte count for logs (1 decimal, binary units).
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
