package extract

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// journal records which archives have been fully extracted so a re-run can
// skip them. Append-only text file: one "done <archive> <files> <bytes> <ts>"
// line per completed archive. Granularity is per-archive: if a run dies
// mid-archive, that archive is re-extracted from the start on resume, which
// is safe because extraction is idempotent (same paths, overwrite).
//
// The merge step (step 2) will use SQLite; this stays deliberately dumber.
type journal struct {
	path string
	done map[string]bool
	f    *os.File // nil in dry-run mode
}

func openJournal(stateDir string, dryRun bool) (*journal, error) {
	j := &journal{
		path: filepath.Join(stateDir, "extract.journal"),
		done: map[string]bool{},
	}

	if b, err := os.ReadFile(j.path); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "done" {
				j.done[fields[1]] = true
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if !dryRun {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		j.f = f
	}
	return j, nil
}

func (j *journal) Done(archive string) bool { return j.done[archive] }

func (j *journal) MarkDone(archive string, files int, bytes int64) error {
	j.done[archive] = true
	if j.f == nil {
		return nil
	}
	_, err := fmt.Fprintf(j.f, "done %s %d %d %s\n",
		archive, files, bytes, time.Now().UTC().Format(time.RFC3339))
	if err == nil {
		err = j.f.Sync()
	}
	return err
}

func (j *journal) Close() error {
	if j.f != nil {
		return j.f.Close()
	}
	return nil
}
