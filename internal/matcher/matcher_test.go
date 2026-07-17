package matcher

import (
	"strings"
	"testing"
)

func TestParseJSONName(t *testing.T) {
	c := DefaultConfig()

	tests := []struct {
		name      string
		json      string
		media     string // expected MediaName
		dup       string
		truncated bool
		notJSON   bool
	}{
		{
			name:  "old plain naming",
			json:  "IMG_0001.jpg.json",
			media: "IMG_0001.jpg",
		},
		{
			name:  "new supplemental naming",
			json:  "IMG_0001.jpg.supplemental-metadata.json",
			media: "IMG_0001.jpg",
		},
		{
			name:  "duplicate index moves before .json",
			json:  "IMG_0001.jpg.supplemental-metadata(1).json",
			media: "IMG_0001.jpg",
			dup:   "1",
		},
		{
			name:  "duplicate index with old naming",
			json:  "IMG_0001.jpg(2).json",
			media: "IMG_0001.jpg",
			dup:   "2",
		},
		{
			// media 44 chars + ".supplemental-metadata" would be 66;
			// Google cuts the base to 46 -> ".s" tail survives.
			name:      "46-char cap cuts into the suffix",
			json:      "PXL_20230115_103045123.PORTRAIT-01.COVER.jpg.s.json",
			media:     "PXL_20230115_103045123.PORTRAIT-01.COVER.jpg",
			truncated: true,
		},
		{
			// media itself longer than 46 -> base is a bare prefix of the
			// media name; nothing to strip, prefix matching must resolve it.
			name:      "46-char cap cuts into the media name",
			json:      strings.Repeat("a", 46) + ".json",
			media:     strings.Repeat("a", 46),
			truncated: true,
		},
		{
			name:  "parenthetical index inside media name is preserved",
			json:  "party(2019).jpg.supplemental-metadata.json",
			media: "party(2019).jpg",
		},
		{
			name:    "not a sidecar",
			json:    "IMG_0001.jpg",
			notJSON: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jn, ok := c.ParseJSONName(tt.json)
			if tt.notJSON {
				if ok {
					t.Fatalf("expected ok=false for %q", tt.json)
				}
				return
			}
			if !ok {
				t.Fatalf("expected ok=true for %q", tt.json)
			}
			if jn.MediaName != tt.media {
				t.Errorf("MediaName = %q, want %q", jn.MediaName, tt.media)
			}
			if jn.DupIndex != tt.dup {
				t.Errorf("DupIndex = %q, want %q", jn.DupIndex, tt.dup)
			}
			if jn.Truncated != tt.truncated {
				t.Errorf("Truncated = %v, want %v", jn.Truncated, tt.truncated)
			}
		})
	}
}

func TestApplyDupIndex(t *testing.T) {
	if got := ApplyDupIndex("IMG.jpg", "1"); got != "IMG(1).jpg" {
		t.Errorf("got %q", got)
	}
	if got := ApplyDupIndex("IMG.jpg", ""); got != "IMG.jpg" {
		t.Errorf("got %q", got)
	}
}

func TestEditedOriginal(t *testing.T) {
	c := DefaultConfig()

	tests := []struct {
		in     string
		orig   string
		edited bool
	}{
		{"IMG_0001-edited.jpg", "IMG_0001.jpg", true},
		{"IMG_0001-modificato.jpg", "IMG_0001.jpg", true}, // Italian export!
		{"IMG_0001.jpg", "IMG_0001.jpg", false},
		{"credited.jpg", "credited.jpg", false}, // suffix must match at stem end only... "credited" does not end with "-edited"
	}
	for _, tt := range tests {
		orig, edited := c.EditedOriginal(tt.in)
		if orig != tt.orig || edited != tt.edited {
			t.Errorf("EditedOriginal(%q) = (%q,%v), want (%q,%v)", tt.in, orig, edited, tt.orig, tt.edited)
		}
	}
}

func TestResolve(t *testing.T) {
	c := DefaultConfig()

	t.Run("exact in same dir", func(t *testing.T) {
		ix := NewIndex()
		ix.AddMedia("/t/Photos from 2019/IMG_1.jpg")
		p, ok := c.Resolve(ix, "/t/Photos from 2019/IMG_1.jpg.supplemental-metadata.json")
		if !ok || p != "/t/Photos from 2019/IMG_1.jpg" {
			t.Fatalf("got (%q,%v)", p, ok)
		}
	})

	t.Run("duplicate index picks the numbered media", func(t *testing.T) {
		ix := NewIndex()
		ix.AddMedia("/t/d/IMG_1.jpg")
		ix.AddMedia("/t/d/IMG_1(1).jpg")
		p, ok := c.Resolve(ix, "/t/d/IMG_1.jpg.supplemental-metadata(1).json")
		if !ok || p != "/t/d/IMG_1(1).jpg" {
			t.Fatalf("got (%q,%v)", p, ok)
		}
		// and the plain sidecar still picks the plain media
		p, ok = c.Resolve(ix, "/t/d/IMG_1.jpg.supplemental-metadata.json")
		if !ok || p != "/t/d/IMG_1.jpg" {
			t.Fatalf("plain: got (%q,%v)", p, ok)
		}
	})

	t.Run("truncated sidecar resolves by prefix", func(t *testing.T) {
		ix := NewIndex()
		media := "PXL_20230115_103045123.PORTRAIT-01.COVER.jpg" // 44 chars
		ix.AddMedia("/t/d/" + media)
		p, ok := c.Resolve(ix, "/t/d/"+media+".s.json") // base = 46 chars
		if !ok || p != "/t/d/"+media {
			t.Fatalf("got (%q,%v)", p, ok)
		}
	})

	t.Run("media name itself truncated", func(t *testing.T) {
		ix := NewIndex()
		long := strings.Repeat("b", 60) + ".jpg"
		ix.AddMedia("/t/d/" + long)
		p, ok := c.Resolve(ix, "/t/d/"+strings.Repeat("b", 46)+".json")
		if !ok || p != "/t/d/"+long {
			t.Fatalf("got (%q,%v)", p, ok)
		}
	})

	t.Run("global fallback across Takeout roots", func(t *testing.T) {
		ix := NewIndex()
		ix.AddMedia("/staging/Takeout 2/Google Photos/Photos from 2020/IMG_9.jpg")
		p, ok := c.Resolve(ix, "/staging/Takeout/Google Photos/Photos from 2020/IMG_9.jpg.supplemental-metadata.json")
		if !ok || !strings.HasSuffix(p, "Takeout 2/Google Photos/Photos from 2020/IMG_9.jpg") {
			t.Fatalf("got (%q,%v)", p, ok)
		}
	})

	t.Run("case-insensitive", func(t *testing.T) {
		ix := NewIndex()
		ix.AddMedia("/t/d/IMG_1.JPG")
		p, ok := c.Resolve(ix, "/t/d/IMG_1.jpg.supplemental-metadata.json")
		if !ok || p != "/t/d/IMG_1.JPG" {
			t.Fatalf("got (%q,%v)", p, ok)
		}
	})

	t.Run("no match", func(t *testing.T) {
		ix := NewIndex()
		ix.AddMedia("/t/d/OTHER.jpg")
		if _, ok := c.Resolve(ix, "/t/d/IMG_1.jpg.supplemental-metadata.json"); ok {
			t.Fatal("expected no match")
		}
	})
}
