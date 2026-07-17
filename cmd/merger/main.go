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

	"github.com/jonathan/takeout-merger/internal/extract"
)

const version = "0.1.0"

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
	input := fs.String("input", "", "staging directory produced by 'merger extract'")
	output := fs.String("output", "", "library output directory (e.g. /volume1/photo)")
	state := fs.String("state", "", "state directory (SQLite database)")
	fs.Bool("dry-run", false, "plan only, write nothing")
	fs.Bool("keep-originals", false, "also keep originals superseded by their edited version")
	fs.Int("workers", 2, "parallel workers")
	_ = fs.Parse(args)
	_ = input
	_ = output
	_ = state

	fmt.Fprintln(os.Stderr, "merge: not implemented yet (step 2 — scanner/matcher/dedup/exiftool writer)")
	os.Exit(2)
}
