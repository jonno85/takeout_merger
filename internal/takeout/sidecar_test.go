package takeout

import (
	"encoding/json"
	"testing"
	"time"
)

const sampleSidecar = `{
  "title": "IMG_20190920_153222.jpg",
  "description": "Tramonto a Torino",
  "photoTakenTime": {"timestamp": "1568991142", "formatted": "20 set 2019, 14:52:22 UTC"},
  "creationTime": {"timestamp": "1569000000"},
  "geoData": {"latitude": 45.0703, "longitude": 7.6869, "altitude": 239.0},
  "geoDataExif": {"latitude": 0.0, "longitude": 0.0, "altitude": 0.0}
}`

func TestSidecarParsing(t *testing.T) {
	var s Sidecar
	if err := json.Unmarshal([]byte(sampleSidecar), &s); err != nil {
		t.Fatal(err)
	}

	if s.Title.String() != "IMG_20190920_153222.jpg" {
		t.Errorf("title = %q", s.Title)
	}

	taken, ok := s.TakenAt()
	if !ok {
		t.Fatal("expected TakenAt ok")
	}
	want := time.Unix(1568991142, 0).UTC()
	if !taken.Equal(want) {
		t.Errorf("TakenAt = %v, want %v (must prefer photoTakenTime over creationTime)", taken, want)
	}

	// geoDataExif is 0/0 -> must fall back to geoData, not report "no location"
	g, ok := s.BestGeo()
	if !ok {
		t.Fatal("expected BestGeo ok via geoData fallback")
	}
	if g.Latitude != 45.0703 || g.Longitude != 7.6869 {
		t.Errorf("geo = %+v", g)
	}
}

func TestZeroGeoIsNoLocation(t *testing.T) {
	var s Sidecar
	if err := json.Unmarshal([]byte(`{"geoData":{"latitude":0,"longitude":0,"altitude":12}}`), &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.BestGeo(); ok {
		t.Error("0/0 coordinates must be treated as no location even with altitude set")
	}
}

func TestTitleAsArray(t *testing.T) {
	var s Sidecar
	if err := json.Unmarshal([]byte(`{"title": ["a.jpg", "ignored"]}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Title.String() != "a.jpg" {
		t.Errorf("title = %q, want first array element", s.Title)
	}
}

func TestMissingTimestamps(t *testing.T) {
	var s Sidecar
	if err := json.Unmarshal([]byte(`{"title": "x.jpg"}`), &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.TakenAt(); ok {
		t.Error("expected no time")
	}
}
