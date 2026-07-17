// Command merger migrates Google Photos Takeout exports into a clean,
// metadata-complete photo library suitable for Synology Photos.
//
// Pipeline (each stage is a separate, resumable step):
//
//	merger extract  # step 1: .tgz archives -> staging tree (PAX-safe)
//	merger merge    # step 2: staging tree  -> library + albums (not yet implemented)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/jonathan/takeout-merger/internal/extract"
	"github.com/jonathan/takeout-merger/internal/merge"
)

const version = "0.2.0"

func main() {
	log.SetFlags(log.Ltime)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "extract":
		cmdExtract(os.Args[2:])
	case "merge":
		cmdMerge(os.Args[2:])
	case "version":
		fmt.Println("merger", version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `merger %s — Google Photos Takeout -> Synology Photos migration

Usage:
  merger extract --archives DIR --staging DIR [--state DIR] [flags]
  merger merge   --input DIR --output DIR --state DIR [flags]   (step 2, WIP)
  merger version

Run "merger <command> -h" for command flags.
`, version)
}

func cmdExtract(args []string) {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	var opts extract.Options
	fs.StringVar(&opts.ArchivesDir, "archives", "", "directory containing takeout-*.tgz / *.tar.gz archives (required)")
	fs.StringVar(&opts.StagingDir, "staging", "", "destination directory for the extracted tree (required)")
	fs.StringVar(&opts.StateDir, "state", "", "directory for the resume journal (default: <staging>/.merger)")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "list archives and entry counts without writing anything")
	fs.BoolVar(&opts.DeleteArchives, "delete-archives", false, "delete each archive after it is fully extracted and verified")
	fs.IntVar(&opts.ProgressEvery, "progress-every", 500, "log progress every N files")
	_ = fs.Parse(args)

	if opts.ArchivesDir == "" || opts.StagingDir == "" {
		fs.Usage()
		os.Exit(2)
	}

	res, err := extract.Run(opts)
	if err != nil {
		log.Fatalf("extract: %v", err)
	}
	log.Printf("extract done: %d archive(s), %d file(s), %s written, %d skipped as already done",
		res.Archives, res.Files, extract.HumanBytes(res.Bytes), res.SkippedArchives)
}

func cmdMerge(args []string) {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	var opts merge.Options
	fs.StringVar(&opts.Input, "input", "", "staging directory produced by 'merger extract' (required)")
	fs.StringVar(&opts.Output, "output", "", "library output directory, e.g. /volume1/photo (required)")
	stateDir := fs.String("state", "", "state directory for the merge journal (default: <output>/.merger)")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "plan only, write nothing")
	fs.BoolVar(&opts.KeepOriginals, "keep-originals", false, "also keep originals superseded by their edited version")
	fs.IntVar(&opts.Workers, "workers", 2, "parallel workers (2 is right for the DS220+)")
	fs.StringVar(&opts.ExiftoolBin, "exiftool", "exiftool", "exiftool binary path, or 'none' to skip metadata embedding")
	_ = fs.Parse(args)

	if opts.Input == "" || opts.Output == "" {
		fs.Usage()
		os.Exit(2)
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(opts.Output, ".merger")
	}
	opts.StatePath = filepath.Join(*stateDir, "merge.state.jsonl")

	if _, err := merge.Run(opts); err != nil {
		log.Fatalf("merge: %v", err)
	}
}
