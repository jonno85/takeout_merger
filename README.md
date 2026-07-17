# takeout-merger

Migrates Google Photos Takeout exports into a clean, metadata-complete photo
library for **Synology Photos** (DS220+, Docker/Container Manager).

Reference for requirements: [SKocur/google-photos-takeout-metadata-merger](https://github.com/SKocur/google-photos-takeout-metadata-merger),
reimplemented headless with extraction, real EXIF for HEIC/MP4 (via ExifTool),
deduplication and incremental re-runs.

## Pipeline

```
step 1  merger extract   .tgz archives ──▶ staging tree      [IMPLEMENTED]
step 2  merger merge     staging tree  ──▶ library + albums  [IMPLEMENTED]
```

Each step is separate and resumable. Future Takeout rounds: drop the new
`.tgz` files into the archives folder and run both steps again — already-seen
content is skipped.

### Step 1 — extract

```
merger extract --archives DIR --staging DIR [--state DIR] [--dry-run] [--delete-archives]
```

* Pure-Go tar/gzip: **PAX extended headers are applied natively**, so long and
  UTF-8 filenames extract intact — no `PaxHeaders.X` junk directories like
  DSM's busybox tar produces. (If your current extracted tree contains
  `PaxHeaders.X`, discard it and re-extract with this tool.)
* Resume journal (`extract.journal` in the state dir): completed archives are
  skipped on re-run. Granularity is per archive; an interrupted archive is
  re-extracted from scratch, which is safe (idempotent overwrite).
* Path sanitization (no absolute paths / `..` traversal), size verification
  per file, mtimes restored from tar headers.
* `--delete-archives` frees disk after each verified archive.

Disk math for a 120 GB export: archives (120) + staging (~120) must fit
simultaneously; add the merged library (~110) if you keep everything.

### Step 2 — merge

```
merger merge --input DIR --output DIR [--state DIR] [--dry-run] [--keep-originals] [--workers N] [--exiftool PATH|none]
```

* Pairs JSON sidecars with media (`internal/matcher`: supplemental-metadata
  naming, 46-char truncation, `(N)` duplicate index relocation,
  `-edited`/`-modificato`, cross-root fallback — all unit-tested).
* Dedup by content hash (SHA-256); the canonical file goes to
  `library/YYYY/MM/` (UTC taken time; `library/undated/` without one).
* Near-duplicates: the **edited** version wins under the original's name and
  inherits the original's sidecar; `--keep-originals` keeps both.
* Live Photos: the video half borrows the photo's sidecar (same stem).
* Albums (folders with `metadata.json` / `metadati.json`) become **hardlinks**
  under `albums/<Title>/` — zero extra space. Links are created only after all
  metadata writes finished (ExifTool replaces files by rename, which would
  otherwise orphan early links).
* EXIF/QuickTime metadata written via **ExifTool `-stay_open` batch mode**:
  DateTimeOriginal/CreateDate + GPS + description for images,
  QuickTime:CreateDate (UTC) + Keys:GPSCoordinates + description for videos.
  One ExifTool process per worker. Metadata failures never lose files —
  they are counted and logged, the copy stays.
* State journal (`merge.state.jsonl`, append-only JSON-lines): re-runs and
  **future Takeout rounds** skip everything already imported; interrupted runs
  resume. Human-readable — `grep` it to answer "why was this skipped?".

## Development (Mac)

```bash
make test            # unit tests, no external deps needed
make build           # native binary
make run-extract-local   # dry-run against a sample archive in ./testdata/archives
```

Tip: request a small fresh Takeout export from Google (a couple of albums) and
keep the `.tgz` in `testdata/archives/` as your end-to-end fixture — it carries
the *current* naming conventions and your account's localization
(`-modificato` for Italian) that the matcher config must match.

## Deployment (Synology)

```bash
make docker
docker save takeout-merger:latest | ssh user@nas "docker load"
# then on the NAS (adjust paths in docker-compose.yml):
docker compose run --rm extract
```

Output target for the merge step is `/volume1/photo` (Synology Photos
**Shared Space** — enable it in Synology Photos ▸ Settings). Folder albums
appear in the mobile app's *Folders* tab.

## Configuration notes

* Go 1.26, pure stdlib — no external dependencies at all. State is an
  append-only JSONL journal (a few MB for 50k files); swap in SQLite later
  only if you ever need ad-hoc querying.
* Matcher tunables (`internal/matcher.DefaultConfig`): metadata suffix names,
  localized edited suffixes, 46-char truncation cap. Verify against your real
  export with:
  `find staging -name '*.json' | sed 's/.*\.\([a-z-]*\)\.json/\1/' | sort | uniq -c`
