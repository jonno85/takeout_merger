// Package merge implements step 2 of the pipeline: staging tree -> library.
//
// Phases:
//
//	scan     walk staging; catalog media, sidecars, album folders
//	plan     resolve sidecars (matcher), group edited variants, hash files
//	execute  dedup against state, copy canonical files to library/YYYY/MM,
//	         write EXIF/QuickTime metadata, hardlink into albums/<Title>,
//	         journal every action for resume + future incremental rounds
package merge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonathan/takeout-merger/internal/exiftool"
	"github.com/jonathan/takeout-merger/internal/matcher"
	"github.com/jonathan/takeout-merger/internal/state"
	"github.com/jonathan/takeout-merger/internal/takeout"
)

// Options configures a merge run.
type Options struct {
	Input          string // staging tree from `merger extract`
	Output         string // library root, e.g. /volume1/photo
	StatePath      string // JSONL journal file
	DryRun         bool
	KeepOriginals  bool // keep originals superseded by their edited version
	Repair         bool // rebuild corrupt metadata structures on write failure
	Workers        int
	ExiftoolBin    string // "" or "none" disables metadata writing
	Matcher        matcher.Config
	AlbumMetaNames []string // filenames marking an album folder
}

// Stats summarizes a run.
type Stats struct {
	MediaSeen     int
	NewFiles      int
	Duplicates    int
	Superseded    int // originals replaced by their edited version
	AlbumLinks    int
	Albums        int
	NoSidecar     int
	UnmatchedJSON int
	MetaErrors    int
	Repaired      int
}

type item struct {
	src     string // absolute path in staging
	name    string // canonical filename to use in the library
	hash    string
	size    int64
	sidecar *takeout.Sidecar
	albums  []string // album titles this media belongs to
	origSrc string   // for edited items: the superseded original's path
}

// Run executes the merge.
func Run(opts Options) (Stats, error) {
	var st Stats
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if len(opts.AlbumMetaNames) == 0 {
		opts.AlbumMetaNames = []string{"metadata.json", "metadati.json"}
	}
	if opts.Matcher.MaxJSONBase == 0 {
		opts.Matcher = matcher.DefaultConfig()
	}

	store, err := state.Open(opts.StatePath, opts.DryRun)
	if err != nil {
		return st, err
	}
	defer store.Close()

	// ---- scan ----
	sc, err := scan(opts)
	if err != nil {
		return st, err
	}
	st.MediaSeen = len(sc.media)
	st.Albums = len(sc.albumTitles)
	log.Printf("scan: %d media, %d sidecars, %d album folder(s)",
		len(sc.media), len(sc.sidecars), len(sc.albumTitles))

	// ---- plan ----
	items, planStats := plan(opts, sc)
	st.Superseded = planStats.superseded
	st.NoSidecar = planStats.noSidecar
	st.UnmatchedJSON = planStats.unmatchedJSON

	m := &merger{
		opts:  opts,
		store: store,
		used:  map[string]bool{},
		canon: map[string]string{},
		stats: &st,
	}
	for h, c := range store.Files {
		m.canon[h] = c
		m.used[strings.ToLower(c)] = true
	}

	// ---- execute ----
	if err := m.executeAll(items); err != nil {
		return st, err
	}
	if err := m.flushAlbumLinks(); err != nil {
		return st, err
	}

	log.Printf("merge%s: %d new, %d duplicate(s), %d superseded by edited, %d album link(s) in %d album(s), %d without sidecar, %d unmatched JSON, %d repaired, %d metadata error(s)",
		dryTag(opts.DryRun), st.NewFiles, st.Duplicates, st.Superseded, st.AlbumLinks, st.Albums, st.NoSidecar, st.UnmatchedJSON, st.Repaired, st.MetaErrors)
	return st, nil
}

// ---------------------------------------------------------------------------
// scan
// ---------------------------------------------------------------------------

type scanResult struct {
	media       []string          // absolute media paths
	sidecars    []string          // absolute sidecar paths
	albumTitles map[string]string // dir -> album title
	index       *matcher.Index
}

func scan(opts Options) (*scanResult, error) {
	sc := &scanResult{
		albumTitles: map[string]string{},
		index:       matcher.NewIndex(),
	}
	albumMeta := map[string]bool{}
	for _, n := range opts.AlbumMetaNames {
		albumMeta[strings.ToLower(n)] = true
	}

	err := filepath.WalkDir(opts.Input, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "PaxHeaders") || name == "@eaDir" {
				return filepath.SkipDir
			}
			return nil
		}
		lower := strings.ToLower(name)
		switch {
		case albumMeta[lower]:
			meta, err := takeout.ReadAlbumMeta(path)
			dir := filepath.Dir(path)
			title := strings.TrimSpace(meta.Title.String())
			if err != nil || title == "" {
				title = filepath.Base(dir)
			}
			sc.albumTitles[dir] = sanitizeName(title)
		case strings.HasSuffix(lower, ".json"):
			sc.sidecars = append(sc.sidecars, path)
		default:
			sc.media = append(sc.media, path)
			sc.index.AddMedia(path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(sc.media) // deterministic ordering
	return sc, nil
}

// ---------------------------------------------------------------------------
// plan
// ---------------------------------------------------------------------------

type planStats struct {
	superseded    int
	noSidecar     int
	unmatchedJSON int
}

func plan(opts Options, sc *scanResult) ([]*item, planStats) {
	var ps planStats

	// Resolve every sidecar to its media file.
	sidecarOf := map[string]string{} // media path -> sidecar path
	for _, jp := range sc.sidecars {
		mp, ok := opts.Matcher.Resolve(sc.index, jp)
		if !ok {
			ps.unmatchedJSON++
			if ps.unmatchedJSON <= 5 {
				log.Printf("warning: no media found for sidecar %s", jp)
			}
			continue
		}
		if _, dup := sidecarOf[mp]; !dup {
			sidecarOf[mp] = jp
		}
	}

	// Group edited variants: original -> edited path (same directory).
	editedOf := map[string]string{}
	for _, mp := range sc.media {
		dir, name := filepath.Split(mp)
		if orig, ok := opts.Matcher.EditedOriginal(name); ok {
			origPath := filepath.Join(filepath.Clean(dir), orig)
			if _, err := os.Stat(origPath); err == nil {
				editedOf[origPath] = mp
			}
		}
	}

	items := make([]*item, 0, len(sc.media))
	for _, mp := range sc.media {
		dir, name := filepath.Split(mp)
		dir = filepath.Clean(dir)

		editedOrigPath := "" // set when mp is an edited variant with a present original
		if orig, ok := opts.Matcher.EditedOriginal(name); ok {
			p := filepath.Join(dir, orig)
			if _, exists := editedOf[p]; exists && p != mp {
				if !opts.KeepOriginals {
					// Folded into the original's item below.
					continue
				}
				editedOrigPath = p
			}
		}

		it := &item{src: mp, name: name}

		// Edited version supersedes the original (bytes from edited, name and
		// sidecar from the original).
		if ed, ok := editedOf[mp]; ok && !opts.KeepOriginals {
			it.origSrc = mp
			it.src = ed
			ps.superseded++
		}

		if jp, ok := sidecarOf[mp]; ok {
			if s, err := takeout.ReadSidecar(jp); err == nil {
				it.sidecar = &s
			}
		} else if editedOrigPath != "" {
			// keep-originals mode: the edited variant has no sidecar of its
			// own; inherit the original's for dates/GPS.
			if jp, ok := sidecarOf[editedOrigPath]; ok {
				if s, err := takeout.ReadSidecar(jp); err == nil {
					it.sidecar = &s
				}
			}
		} else if s := borrowSidecar(opts, sidecarOf, dir, name); s != nil {
			// Live Photo pairing: HEIC's sidecar covers the paired MP4.
			it.sidecar = s
		} else {
			ps.noSidecar++
		}

		if title, ok := sc.albumTitles[dir]; ok {
			it.albums = append(it.albums, title)
		}
		items = append(items, it)
	}

	// --keep-originals: superseded originals become regular items too, so
	// nothing extra to do — they were never removed from the list above.
	return items, ps
}

// borrowSidecar finds a same-stem sibling's sidecar (Live Photos: the video
// half has no JSON of its own).
func borrowSidecar(opts Options, sidecarOf map[string]string, dir, name string) *takeout.Sidecar {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	for mp, jp := range sidecarOf {
		d, n := filepath.Split(mp)
		if filepath.Clean(d) != dir {
			continue
		}
		if strings.EqualFold(strings.TrimSuffix(n, filepath.Ext(n)), stem) && !strings.EqualFold(n, name) {
			if s, err := takeout.ReadSidecar(jp); err == nil {
				return s2ptr(s)
			}
		}
	}
	return nil
}

func s2ptr(s takeout.Sidecar) *takeout.Sidecar { return &s }

// ---------------------------------------------------------------------------
// execute
// ---------------------------------------------------------------------------

type merger struct {
	opts  Options
	store *state.Store
	stats *Stats

	mu    sync.Mutex
	used  map[string]bool   // lowercase canonical rel paths in use
	canon map[string]string // hash -> canonical rel path (state + this run)

	// Album hardlinks are deferred until every canonical file is fully
	// written: exiftool's -overwrite_original replaces files via rename
	// (new inode), so a link created while another worker is still writing
	// metadata would keep the metadata-less bytes alive.
	pending []linkTask
}

type linkTask struct {
	hash   string
	albums []string
}

func (m *merger) executeAll(items []*item) error {
	jobs := make(chan *item)
	errs := make(chan error, m.opts.Workers)

	var wg sync.WaitGroup
	for w := 0; w < m.opts.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			writer, err := m.newWriter()
			if err != nil {
				errs <- err
				return
			}
			if writer != nil {
				defer writer.Close()
			}
			for it := range jobs {
				if err := m.process(it, writer); err != nil {
					errs <- fmt.Errorf("%s: %w", it.src, err)
					return
				}
			}
		}()
	}

	var sendErr error
send:
	for _, it := range items {
		select {
		case jobs <- it:
		case sendErr = <-errs:
			break send
		}
	}
	close(jobs)
	wg.Wait()
	if sendErr != nil {
		return sendErr
	}
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

func (m *merger) newWriter() (exiftool.Writer, error) {
	if m.opts.DryRun || m.opts.ExiftoolBin == "" || m.opts.ExiftoolBin == "none" {
		return nil, nil
	}
	return exiftool.Start(m.opts.ExiftoolBin)
}

func (m *merger) process(it *item, writer exiftool.Writer) error {
	h, size, header, err := hashFile(it.src)
	if err != nil {
		return err
	}
	it.hash, it.size = h, size

	// Google Photos "storage saver" transcodes HEIC->JPEG but Takeout keeps
	// the .HEIC filename; exiftool then refuses to write ("Not a valid HEIC,
	// looks more like a JPEG") and Synology Photos indexes it wrong. Detect
	// the real format from magic bytes and correct the canonical extension.
	if fixed, changed := correctedName(it.name, header); changed {
		log.Printf("extension corrected: %s is really %s", it.name, filepath.Ext(fixed))
		it.name = fixed
	}

	m.mu.Lock()
	canonical, dup := m.canon[it.hash]
	if !dup {
		canonical = m.allocCanonicalLocked(it)
		m.canon[it.hash] = canonical
	}
	m.mu.Unlock()

	if dup {
		m.count(func(s *Stats) { s.Duplicates++ })
		if !m.opts.DryRun {
			if err := m.store.AddSkip(it.src, "duplicate"); err != nil {
				return err
			}
		}
		m.queueLinks(it)
		return nil
	}

	// New canonical file.
	if !m.opts.DryRun {
		dst := filepath.Join(m.opts.Output, canonical)
		if err := copyFile(it.src, dst); err != nil {
			return err
		}
		if it.origSrc != "" {
			if err := m.store.AddSkip(it.origSrc, "superseded-by-edited"); err != nil {
				return err
			}
		}

		meta, taken := metaFor(it)
		if writer != nil {
			if err := writer.Write(dst, meta); err != nil {
				repaired := false
				if m.opts.Repair {
					if rerr := writer.Repair(dst, meta); rerr == nil {
						repaired = true
						log.Printf("repaired corrupt metadata: %s", canonical)
						m.count(func(s *Stats) { s.Repaired++ })
					} else {
						log.Printf("warning: repair also failed for %s: %v", canonical, rerr)
					}
				}
				if !repaired {
					// Metadata failure must not lose the file: keep the copy,
					// count the error, continue. (Filesystem mtime still set.)
					log.Printf("warning: metadata write failed for %s: %v", canonical, err)
					m.count(func(s *Stats) { s.MetaErrors++ })
				}
			}
		}
		if !taken.IsZero() {
			_ = os.Chtimes(dst, taken, taken)
		}
		if err := m.store.AddFile(it.hash, canonical, it.src, taken); err != nil {
			return err
		}
	}
	m.count(func(s *Stats) { s.NewFiles++ })
	m.queueLinks(it)
	return nil
}

func (m *merger) queueLinks(it *item) {
	if len(it.albums) == 0 {
		return
	}
	m.mu.Lock()
	m.pending = append(m.pending, linkTask{hash: it.hash, albums: it.albums})
	m.mu.Unlock()
}

// allocCanonicalLocked picks a unique library-relative path. Caller holds mu.
func (m *merger) allocCanonicalLocked(it *item) string {
	sub := "library/undated"
	if it.sidecar != nil {
		if t, ok := it.sidecar.TakenAt(); ok {
			sub = "library/" + t.UTC().Format("2006/01")
		}
	}
	base := it.name
	if it.origSrc != "" && m.opts.KeepOriginals {
		// (unused path today: origSrc is only set when originals are dropped)
		base = it.name
	}
	cand := filepath.ToSlash(filepath.Join(sub, base))
	if !m.used[strings.ToLower(cand)] {
		m.used[strings.ToLower(cand)] = true
		return cand
	}
	// Name collision with different content: disambiguate with hash prefix.
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 0; ; i++ {
		suffix := it.hash[:8]
		if i > 0 {
			suffix = fmt.Sprintf("%s-%d", it.hash[:8], i)
		}
		cand = filepath.ToSlash(filepath.Join(sub, stem+"-"+suffix+ext))
		if !m.used[strings.ToLower(cand)] {
			m.used[strings.ToLower(cand)] = true
			return cand
		}
	}
}

// flushAlbumLinks runs after all workers finished: every canonical file has
// its final bytes and inode, so hardlinks are now stable.
func (m *merger) flushAlbumLinks() error {
	for _, task := range m.pending {
		canonical, ok := m.canon[task.hash]
		if !ok {
			continue // defensive; cannot happen
		}
		for _, album := range task.albums {
			if m.store.HasAlbumLink(task.hash, album) {
				continue
			}
			link := filepath.ToSlash(filepath.Join("albums", album, filepath.Base(canonical)))
			if !m.opts.DryRun {
				src := filepath.Join(m.opts.Output, canonical)
				dst := filepath.Join(m.opts.Output, link)
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					return err
				}
				if err := os.Link(src, dst); err != nil {
					if os.IsExist(err) {
						// already linked (e.g. previous crashed run) — fine
					} else {
						// Cross-filesystem or FS without hardlinks: fall back to copy.
						log.Printf("warning: hardlink failed (%v), copying instead", err)
						if err := copyFile(src, dst); err != nil {
							return err
						}
					}
				}
				if err := m.store.AddAlbum(task.hash, album, link); err != nil {
					return err
				}
			}
			m.count(func(s *Stats) { s.AlbumLinks++ })
		}
	}
	return nil
}

func metaFor(it *item) (exiftool.Meta, time.Time) {
	var meta exiftool.Meta
	var taken time.Time
	if it.sidecar == nil {
		return meta, taken
	}
	if t, ok := it.sidecar.TakenAt(); ok {
		meta.TakenAt = t
		taken = t
	}
	if g, ok := it.sidecar.BestGeo(); ok {
		meta.HasGeo = true
		meta.Lat, meta.Lng, meta.Alt = g.Latitude, g.Longitude, g.Altitude
	}
	meta.Description = it.sidecar.Description.String()
	return meta, taken
}

func (m *merger) count(f func(*Stats)) {
	m.mu.Lock()
	f(m.stats)
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func hashFile(path string) (string, int64, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, nil, err
	}
	defer f.Close()
	header := make([]byte, 32)
	hn, err := io.ReadFull(f, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", 0, nil, err
	}
	header = header[:hn]
	h := sha256.New()
	h.Write(header)
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), n + int64(hn), header, nil
}

// sniffExt detects the real format from magic bytes; "" when unknown or not
// one of the formats we care about. Video containers included: the same
// lying-extension disease exists there (.mp4 files that are really MKV).
func sniffExt(header []byte) string {
	switch {
	case len(header) >= 3 && header[0] == 0xFF && header[1] == 0xD8 && header[2] == 0xFF:
		return "jpg"
	case len(header) >= 8 && string(header[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(header) >= 6 && (string(header[:6]) == "GIF87a" || string(header[:6]) == "GIF89a"):
		return "gif"
	case len(header) >= 4 && header[0] == 0x1A && header[1] == 0x45 && header[2] == 0xDF && header[3] == 0xA3:
		return "mkv" // EBML: Matroska or WebM (indistinguishable this shallow)
	case len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "AVI ":
		return "avi"
	case len(header) >= 12 && string(header[4:8]) == "ftyp":
		brand := string(header[8:12])
		switch brand {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			return "heic"
		case "qt  ":
			return "mov"
		default:
			// isom, iso2, mp41, mp42, mp4v, avc1, M4V , 3gp*, ...
			return "mp4"
		}
	}
	return ""
}

// extGroups: extensions that are interchangeable enough that a sniff result
// in the same group must NOT trigger a rename (mov vs mp4, mkv vs webm).
var extGroups = map[string]string{
	"jpg": "jpg", "jpeg": "jpg",
	"png":  "png",
	"gif":  "gif",
	"heic": "heic", "heif": "heic",
	"mp4": "mp4family", "m4v": "mp4family", "mov": "mp4family", "3gp": "mp4family",
	"mkv": "ebml", "webm": "ebml",
	"avi": "avi",
}

var sniffGroups = map[string]string{
	"jpg": "jpg", "png": "png", "gif": "gif", "heic": "heic",
	"mp4": "mp4family", "mov": "mp4family", "mkv": "ebml", "avi": "avi",
}

// correctedName fixes the extension when the content contradicts it. Only
// known extensions are ever touched, and only when the sniffed format is
// confidently known and genuinely different (not just a sibling container).
func correctedName(name string, header []byte) (string, bool) {
	real := sniffExt(header)
	if real == "" {
		return name, false
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	currentGroup, known := extGroups[ext]
	if !known {
		return name, false // never touch extensions we don't understand
	}
	if currentGroup == sniffGroups[real] {
		return name, false
	}
	return strings.TrimSuffix(name, filepath.Ext(name)) + "." + real, true
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// sanitizeName makes an album title safe as a directory name on ext4/btrfs
// and SMB clients.
func sanitizeName(s string) string {
	repl := strings.NewReplacer(
		"/", "-", `\`, "-", ":", "-", "*", "-", "?", "-",
		`"`, "'", "<", "(", ">", ")", "|", "-", "\x00", "",
	)
	s = strings.TrimSpace(repl.Replace(s))
	s = strings.Trim(s, ". ")
	if s == "" {
		s = "Untitled"
	}
	return s
}

func dryTag(dry bool) string {
	if dry {
		return " [dry-run]"
	}
	return ""
}
