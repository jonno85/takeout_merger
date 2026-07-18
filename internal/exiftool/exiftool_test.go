package exiftool

import (
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// makeJPEG writes a tiny real JPEG so exiftool has something valid to edit.
func makeJPEG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAndReadBack(t *testing.T) {
	bin, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not installed; skipping integration test")
	}

	dir := t.TempDir()
	jp := filepath.Join(dir, "torino ù.jpg") // UTF-8 + space in name on purpose
	makeJPEG(t, jp)

	tool, err := Start(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer tool.Close()

	taken := time.Date(2019, 9, 20, 14, 52, 22, 0, time.UTC)
	meta := Meta{
		TakenAt:     taken,
		HasGeo:      true,
		Lat:         45.0703,
		Lng:         7.6869,
		Alt:         239,
		Description: "Tramonto a Torino",
	}
	if err := tool.Write(jp, meta); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Second file through the same process proves stay_open batching works.
	jp2 := filepath.Join(dir, "second.jpg")
	makeJPEG(t, jp2)
	if err := tool.Write(jp2, Meta{TakenAt: taken}); err != nil {
		t.Fatalf("Write second: %v", err)
	}

	// Repair path: full metadata rebuild + rewrite through the same process.
	jp3 := filepath.Join(dir, "repair-me.jpg")
	makeJPEG(t, jp3)
	if err := tool.Repair(jp3, meta); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	out3, err := exec.Command(bin, "-j", "-DateTimeOriginal", jp3).Output()
	if err != nil {
		t.Fatal(err)
	}
	var res3 []map[string]any
	if err := json.Unmarshal(out3, &res3); err != nil {
		t.Fatal(err)
	}
	if res3[0]["DateTimeOriginal"] != "2019:09:20 14:52:22" {
		t.Errorf("after Repair, DateTimeOriginal = %v", res3[0]["DateTimeOriginal"])
	}

	// Read back with a fresh exiftool invocation.
	out, err := exec.Command(bin, "-j", "-n",
		"-DateTimeOriginal", "-GPSLatitude", "-GPSLongitude", "-ImageDescription", jp).Output()
	if err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	got := res[0]

	if got["DateTimeOriginal"] != "2019:09:20 14:52:22" {
		t.Errorf("DateTimeOriginal = %v", got["DateTimeOriginal"])
	}
	lat, _ := got["GPSLatitude"].(float64)
	lng, _ := got["GPSLongitude"].(float64)
	if lat < 45.06 || lat > 45.08 || lng < 7.68 || lng > 7.69 {
		t.Errorf("GPS = %v, %v", got["GPSLatitude"], got["GPSLongitude"])
	}
	if got["ImageDescription"] != "Tramonto a Torino" {
		t.Errorf("ImageDescription = %v", got["ImageDescription"])
	}
}

func TestNoTagsIsNoop(t *testing.T) {
	if args := buildArgs("x.jpg", Meta{}); args != nil {
		t.Errorf("expected nil args for empty meta, got %v", args)
	}
}

func TestRepairRebuildsAndWrites(t *testing.T) {
	bin, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not installed; skipping integration test")
	}

	dir := t.TempDir()
	jp := filepath.Join(dir, "corruptish.jpg")
	makeJPEG(t, jp)

	tool, err := Start(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer tool.Close()

	taken := time.Date(2017, 8, 21, 11, 25, 59, 0, time.UTC)
	if err := tool.Repair(jp, Meta{TakenAt: taken, HasGeo: true, Lat: 45.07, Lng: 7.68, Alt: 239}); err != nil {
		t.Fatalf("Repair: %v", err)
	}

	out, err := exec.Command(bin, "-j", "-n", "-DateTimeOriginal", "-GPSLatitude", jp).Output()
	if err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res[0]["DateTimeOriginal"] != "2017:08:21 11:25:59" {
		t.Errorf("DateTimeOriginal after repair = %v", res[0]["DateTimeOriginal"])
	}
	if lat, _ := res[0]["GPSLatitude"].(float64); lat < 45.06 || lat > 45.08 {
		t.Errorf("GPSLatitude after repair = %v", res[0]["GPSLatitude"])
	}
}

func TestUnwritableContainersSkipped(t *testing.T) {
	for _, name := range []string{"a.avi", "b.mkv", "c.wmv", "d.mpg"} {
		if args := buildArgs(name, Meta{TakenAt: time.Now()}); args != nil {
			t.Errorf("%s: expected nil args (unwritable), got %v", name, args)
		}
	}
}

func TestGIFUsesXMPOnly(t *testing.T) {
	taken := time.Date(2014, 2, 1, 0, 0, 0, 0, time.UTC)
	args := buildArgs("anim.gif", Meta{TakenAt: taken, HasGeo: true, Lat: 1, Lng: 2, Alt: 3, Description: "d"})
	joined := ""
	for _, a := range args {
		joined += a + "\n"
	}
	if contains(joined, "EXIF:") {
		t.Errorf("GIF args must not contain EXIF tags:\n%s", joined)
	}
	for _, want := range []string{"XMP-exif:DateTimeOriginal=2014:02:01", "XMP-exif:GPSLatitude", "XMP-dc:Description=d"} {
		if !contains(joined, want) {
			t.Errorf("GIF args missing %q:\n%s", want, joined)
		}
	}
}

func TestVideoTagSelection(t *testing.T) {
	taken := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	args := buildArgs("clip.MP4", Meta{TakenAt: taken, HasGeo: true, Lat: 1, Lng: 2, Alt: 3})
	joined := ""
	for _, a := range args {
		joined += a + "\n"
	}
	for _, want := range []string{"QuickTime:CreateDate=2020:01:02 03:04:05", "Keys:GPSCoordinates", "QuickTimeUTC=1"} {
		if !contains(joined, want) {
			t.Errorf("video args missing %q in:\n%s", want, joined)
		}
	}
	if contains(joined, "EXIF:") {
		t.Errorf("video args must not carry EXIF tags:\n%s", joined)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
