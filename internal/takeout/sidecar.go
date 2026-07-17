// Package takeout models the JSON files Google Takeout ships next to media:
// per-photo sidecars and per-album metadata.json.
package takeout

import (
	"encoding/json"
	"os"
	"strconv"
	"time"
)

// Sidecar is a per-media JSON file (IMG_x.jpg.supplemental-metadata.json).
type Sidecar struct {
	Title          FlexString `json:"title"` // sometimes an array — hence FlexString
	Description    FlexString `json:"description"`
	PhotoTakenTime Timestamp  `json:"photoTakenTime"`
	CreationTime   Timestamp  `json:"creationTime"`
	GeoData        Geo        `json:"geoData"`
	GeoDataExif    Geo        `json:"geoDataExif"`
}

// BestGeo prefers geoDataExif (from the original camera EXIF) and falls back
// to geoData (Google's estimate). Returns ok=false when both are 0/0, which
// Takeout uses as "no location" and must never be written into EXIF.
func (s Sidecar) BestGeo() (Geo, bool) {
	if !s.GeoDataExif.IsZero() {
		return s.GeoDataExif, true
	}
	if !s.GeoData.IsZero() {
		return s.GeoData, true
	}
	return Geo{}, false
}

// TakenAt prefers photoTakenTime over creationTime (upload time).
// The value is UTC — Takeout does not preserve the original timezone.
func (s Sidecar) TakenAt() (time.Time, bool) {
	if t, ok := s.PhotoTakenTime.Time(); ok {
		return t, true
	}
	return s.CreationTime.Time()
}

// AlbumMeta is an album folder's metadata.json.
type AlbumMeta struct {
	Title       FlexString `json:"title"`
	Description FlexString `json:"description"`
	Access      string     `json:"access"`
}

// Geo is a Takeout coordinate triple.
type Geo struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Altitude  float64 `json:"altitude"`
}

// IsZero reports the 0/0 "no location" marker (altitude alone is not enough
// to consider a location valid).
func (g Geo) IsZero() bool { return g.Latitude == 0 && g.Longitude == 0 }

// Timestamp is Takeout's {"timestamp": "1568991142", "formatted": "..."}.
type Timestamp struct {
	Timestamp string `json:"timestamp"`
}

// Time converts the epoch-seconds string; ok=false when absent or invalid.
func (t Timestamp) Time() (time.Time, bool) {
	if t.Timestamp == "" {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(t.Timestamp, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).UTC(), true
}

// FlexString unmarshals a JSON string OR an array of strings (Takeout emits
// "title": ["..."] on some assets). Arrays collapse to their first element.
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = FlexString(s)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		if len(arr) > 0 {
			*f = FlexString(arr[0])
		}
		return nil
	}
	// Unknown shape: tolerate rather than fail the whole file.
	*f = ""
	return nil
}

func (f FlexString) String() string { return string(f) }

// ReadSidecar loads and parses one sidecar file.
func ReadSidecar(path string) (Sidecar, error) {
	var s Sidecar
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

// ReadAlbumMeta loads an album folder's metadata.json.
func ReadAlbumMeta(path string) (AlbumMeta, error) {
	var a AlbumMeta
	b, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	err = json.Unmarshal(b, &a)
	return a, err
}
