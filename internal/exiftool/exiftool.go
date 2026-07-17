// Package exiftool drives a persistent ExifTool process in -stay_open batch
// mode. Spawning exiftool per file costs ~150ms of Perl startup; the batch
// pipe brings that to ~a few ms per file, which matters at 50k files on a
// Celeron.
package exiftool

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Meta is the metadata to embed into a media file.
type Meta struct {
	TakenAt     time.Time // zero = don't write dates
	HasGeo      bool
	Lat, Lng    float64
	Alt         float64
	Description string
}

// Writer is what the merge step needs; *Tool implements it. Tests use fakes.
type Writer interface {
	Write(path string, m Meta) error
	Close() error
}

// Tool is one persistent exiftool process. Not safe for concurrent use;
// create one Tool per worker.
type Tool struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *prefixBuffer
}

// Start launches exiftool (bin is the executable name or path).
func Start(bin string) (*Tool, error) {
	cmd := exec.Command(bin, "-stay_open", "True", "-@", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	eb := &prefixBuffer{}
	cmd.Stderr = eb
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w (is exiftool installed?)", bin, err)
	}
	return &Tool{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		stderr: eb,
	}, nil
}

// Write embeds m into the file at path. Errors include exiftool's own
// message (e.g. unsupported format, corrupt file).
func (t *Tool) Write(path string, m Meta) error {
	args := buildArgs(path, m)
	if len(args) == 0 {
		return nil // nothing to write
	}
	out, err := t.execute(args)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "1 image files updated") &&
		!strings.Contains(out, "1 video files updated") &&
		!strings.Contains(out, "1 files updated") {
		return fmt.Errorf("exiftool did not update %s: %s%s", path, strings.TrimSpace(out), t.stderr.Drain())
	}
	return nil
}

// execute sends one command (args + -execute) and reads until {ready}.
func (t *Tool) execute(args []string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var b bytes.Buffer
	for _, a := range args {
		b.WriteString(a)
		b.WriteByte('\n')
	}
	b.WriteString("-execute\n")
	if _, err := t.stdin.Write(b.Bytes()); err != nil {
		return "", fmt.Errorf("exiftool pipe: %w", err)
	}

	var out strings.Builder
	for {
		line, err := t.stdout.ReadString('\n')
		if err != nil {
			return out.String(), fmt.Errorf("exiftool died: %w%s", err, t.stderr.Drain())
		}
		if strings.HasPrefix(line, "{ready}") {
			return out.String(), nil
		}
		out.WriteString(line)
	}
}

func buildArgs(path string, m Meta) []string {
	video := isVideo(path)

	var args []string
	add := func(a ...string) { args = append(args, a...) }

	// Common flags. -m: tolerate minor errors (Takeout media is full of
	// slightly out-of-spec files). -P is NOT used: we chtimes explicitly.
	add("-charset", "filename=UTF8", "-overwrite_original", "-m")
	if video {
		// QuickTime spec says dates are UTC; without this exiftool would
		// interpret our value as local time of the NAS.
		add("-api", "QuickTimeUTC=1")
	}

	n := len(args)

	if !m.TakenAt.IsZero() {
		ts := m.TakenAt.UTC().Format("2006:01:02 15:04:05")
		if video {
			add("-QuickTime:CreateDate="+ts, "-QuickTime:ModifyDate="+ts)
		} else {
			add("-EXIF:DateTimeOriginal="+ts, "-EXIF:CreateDate="+ts)
		}
	}

	if m.HasGeo {
		if video {
			add(fmt.Sprintf("-Keys:GPSCoordinates=%f, %f, %f", m.Lat, m.Lng, m.Alt))
		} else {
			latRef, lngRef := "N", "E"
			if m.Lat < 0 {
				latRef = "S"
			}
			if m.Lng < 0 {
				lngRef = "W"
			}
			altRef := "0" // above sea level
			if m.Alt < 0 {
				altRef = "1"
			}
			add(
				fmt.Sprintf("-EXIF:GPSLatitude=%f", abs(m.Lat)),
				"-EXIF:GPSLatitudeRef="+latRef,
				fmt.Sprintf("-EXIF:GPSLongitude=%f", abs(m.Lng)),
				"-EXIF:GPSLongitudeRef="+lngRef,
				fmt.Sprintf("-EXIF:GPSAltitude=%f", abs(m.Alt)),
				"-EXIF:GPSAltitudeRef="+altRef,
			)
		}
	}

	if m.Description != "" {
		if video {
			add("-Keys:Description=" + m.Description)
		} else {
			add("-EXIF:ImageDescription="+m.Description, "-XMP-dc:Description="+m.Description)
		}
	}

	if len(args) == n {
		return nil // no tags to write
	}
	return append(args, path)
}

func isVideo(path string) bool {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return false
	}
	switch strings.ToLower(path[i+1:]) {
	case "mp4", "mov", "m4v", "3gp", "avi", "mpg", "mpeg", "mts", "m2ts", "wmv", "mkv", "webm":
		return true
	}
	return false
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// Close shuts the exiftool process down cleanly.
func (t *Tool) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stdin != nil {
		_, _ = io.WriteString(t.stdin, "-stay_open\nFalse\n")
		t.stdin.Close()
		t.stdin = nil
	}
	if t.cmd != nil {
		err := t.cmd.Wait()
		t.cmd = nil
		return err
	}
	return nil
}

// prefixBuffer collects stderr; Drain returns and clears it, prefixed for
// error message readability.
type prefixBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (p *prefixBuffer) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.b.Write(b)
}

func (p *prefixBuffer) Drain() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := strings.TrimSpace(p.b.String())
	p.b.Reset()
	if s == "" {
		return ""
	}
	return "; stderr: " + s
}
