// Package matcher pairs Google Takeout JSON sidecars with their media files.
//
// This is the riskiest part of the whole migration, because Takeout's naming
// is full of quirks:
//
//  1. Plain:       IMG_001.jpg          -> IMG_001.jpg.json
//  2. New naming:  IMG_001.jpg          -> IMG_001.jpg.supplemental-metadata.json
//  3. 46-char cap: the JSON base name (everything before the final ".json")
//     is truncated to 46 characters, cutting into the suffix or even into
//     the media filename itself.
//  4. Duplicates:  IMG_001(1).jpg       -> IMG_001.jpg.supplemental-metadata(1).json
//     (the "(1)" moves to just before ".json"!)
//  5. Edited:      IMG_001-edited.jpg / IMG_001-modificato.jpg share the
//     ORIGINAL's sidecar (edited files have none of their own).
//
// Everything here is a pure function over names plus an in-memory Index, so
// it is trivially unit-testable without touching a real export.
package matcher

import (
	"path/filepath"
	"sort"
	"strings"
)

// Config holds the tunables that vary between exports (mainly localization).
type Config struct {
	// MetadataSuffixes are sidecar name suffixes (without the leading dot and
	// without ".json"). Order matters: longest first.
	MetadataSuffixes []string
	// EditedSuffixes mark edited variants, localized by Google account
	// language ("-edited" for English, "-modificato" for Italian, ...).
	EditedSuffixes []string
	// MaxJSONBase is the truncation cap Google applies to the JSON base name.
	MaxJSONBase int
}

// DefaultConfig matches current (2025/2026) Takeout exports.
// Verify EditedSuffixes against your real export before the big run.
func DefaultConfig() Config {
	return Config{
		MetadataSuffixes: []string{"supplemental-metadata", "supplemental-metadat"},
		EditedSuffixes:   []string{"-edited", "-modificato", "-bearbeitet", "-modifié"},
		MaxJSONBase:      46,
	}
}

// JSONName is the decomposition of a sidecar filename.
type JSONName struct {
	// MediaName is the media filename the sidecar refers to. If Truncated is
	// true it is only a PREFIX of the real media filename.
	MediaName string
	// DupIndex is the "(N)" duplicate marker found before ".json", already
	// stripped from MediaName ("" when absent). Apply it with ApplyDupIndex
	// once the full media name is known.
	DupIndex string
	// Truncated reports that the base name hit Config.MaxJSONBase, so exact
	// matching may fail and prefix matching is required.
	Truncated bool
}

// ParseJSONName decomposes a sidecar filename (base name, not a path).
// ok is false when name is not a .json file.
func (c Config) ParseJSONName(name string) (jn JSONName, ok bool) {
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".json") {
		return jn, false
	}
	base := name[:len(name)-len(".json")]

	jn.Truncated = len(base) >= c.maxBase()

	// (4) duplicate index directly before ".json"
	if i := strings.LastIndex(base, "("); i >= 0 && strings.HasSuffix(base, ")") {
		idx := base[i+1 : len(base)-1]
		if isDigits(idx) {
			jn.DupIndex = idx
			base = base[:i]
		}
	}

	// (2) strip the metadata suffix, tolerating (3) truncation: the tail
	// after the last dot may be any non-empty prefix of a known suffix.
	if i := strings.LastIndex(base, "."); i >= 0 {
		tail := base[i+1:]
		for _, s := range c.MetadataSuffixes {
			if tail == s || (jn.Truncated && len(tail) > 0 && strings.HasPrefix(s, tail)) {
				base = base[:i]
				break
			}
		}
	}

	jn.MediaName = base
	return jn, true
}

// ApplyDupIndex inserts "(N)" before the media extension:
// ("IMG.jpg", "1") -> "IMG(1).jpg". With an empty index it returns mediaName.
func ApplyDupIndex(mediaName, dupIndex string) string {
	if dupIndex == "" {
		return mediaName
	}
	ext := filepath.Ext(mediaName)
	stem := strings.TrimSuffix(mediaName, ext)
	return stem + "(" + dupIndex + ")" + ext
}

// EditedOriginal reports whether name is an edited variant and returns the
// original media filename it derives from:
// "IMG-modificato.jpg" -> ("IMG.jpg", true).
func (c Config) EditedOriginal(name string) (orig string, edited bool) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for _, s := range c.EditedSuffixes {
		if strings.HasSuffix(stem, s) {
			return strings.TrimSuffix(stem, s) + ext, true
		}
	}
	return name, false
}

func (c Config) maxBase() int {
	if c.MaxJSONBase > 0 {
		return c.MaxJSONBase
	}
	return 46
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Index: media catalog + sidecar resolution
// ---------------------------------------------------------------------------

// Index catalogs media files (every non-JSON file) across all Takeout roots.
// Lookup is case-insensitive because Takeout's case handling is inconsistent
// between the sidecar name and the media entry.
type Index struct {
	// dir (as given) -> lowercase media name -> actual path
	byDir map[string]map[string]string
	// lowercase media name -> all paths, for cross-root ("global") fallback:
	// JSON and media of the same asset can land in different "Takeout N"
	// archives when Google splits the export.
	byName map[string][]string
}

func NewIndex() *Index {
	return &Index{
		byDir:  map[string]map[string]string{},
		byName: map[string][]string{},
	}
}

// AddMedia registers a media file path in the catalog.
func (ix *Index) AddMedia(path string) {
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	key := strings.ToLower(name)
	m := ix.byDir[dir]
	if m == nil {
		m = map[string]string{}
		ix.byDir[dir] = m
	}
	m[key] = path
	ix.byName[key] = append(ix.byName[key], path)
}

// Resolve finds the media file a sidecar refers to. Strategy, in order:
//
//  1. exact name in the sidecar's own directory
//  2. truncation-aware prefix match in the same directory
//  3. exact name anywhere across roots (global fallback)
//  4. prefix match anywhere across roots
//
// The dup index is applied before each exact attempt.
func (c Config) Resolve(ix *Index, jsonPath string) (mediaPath string, ok bool) {
	dir, jsonName := filepath.Split(jsonPath)
	dir = filepath.Clean(dir)
	jn, isJSON := c.ParseJSONName(jsonName)
	if !isJSON {
		return "", false
	}

	exact := strings.ToLower(ApplyDupIndex(jn.MediaName, jn.DupIndex))

	// 1. same dir, exact
	if m := ix.byDir[dir]; m != nil {
		if p, hit := m[exact]; hit {
			return p, true
		}
		// 2. same dir, truncated prefix
		if jn.Truncated {
			if p, hit := prefixMatch(m, strings.ToLower(jn.MediaName), jn.DupIndex); hit {
				return p, true
			}
		}
	}

	// 3. global, exact
	if ps := ix.byName[exact]; len(ps) > 0 {
		return ps[0], true
	}

	// 4. global, truncated prefix
	if jn.Truncated {
		all := map[string]string{}
		for k, ps := range ix.byName {
			all[k] = ps[0]
		}
		if p, hit := prefixMatch(all, strings.ToLower(jn.MediaName), jn.DupIndex); hit {
			return p, true
		}
	}

	return "", false
}

// prefixMatch finds media names starting with prefix. When dupIndex is set,
// candidates must carry the matching "(N)" marker; with several candidates
// the shortest name wins (deterministic, and the longer ones belong to other
// assets sharing the prefix).
func prefixMatch(m map[string]string, prefix, dupIndex string) (string, bool) {
	var names []string
	for name := range m {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if dupIndex != "" && !strings.Contains(name, "("+dupIndex+")") {
			continue
		}
		if dupIndex == "" && looksNumbered(name) && !looksNumbered(prefix) {
			continue // don't let IMG(1).jpg answer for IMG.jpg's sidecar
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) < len(names[j])
		}
		return names[i] < names[j]
	})
	return m[names[0]], true
}

func looksNumbered(name string) bool {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if !strings.HasSuffix(stem, ")") {
		return false
	}
	i := strings.LastIndex(stem, "(")
	return i >= 0 && isDigits(stem[i+1:len(stem)-1])
}
