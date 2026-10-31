# Third-party notices

Kinara is licensed under the GNU Affero General Public License v3.0 (see
LICENSE). It includes code adapted from these projects.

## Silo

Subtitle sync (lining a subtitle up with a film's speech) is adapted from
Silo, https://github.com/Silo-Server/silo-server, at commit
`01e01b0417d2937ed577ce996ce1ef5ae108311e` (October 16, 2026).

- License: GNU Affero General Public License v3.0
- Copyright (C) the Silo authors
- Where: `internal/subsync/` in this repository. Each adapted file names the
  Silo file it came from and what Kinara changed.
  - `align.go`, `align_test.go`, `fft.go`: from
    `internal/subtitles/subsync/`, with package-local types.
  - `speech.go`: from `internal/mediasample/speech.go` and
    `internal/subtitles/subsync/speech.go`, running ffmpeg directly.
  - `timing.go`: from `internal/subtitles/cues.go` and
    `internal/subtitles/timing.go`, WebVTT only.
- How it works is described in Silo's `docs/architecture/subtitle-sync.md`.
