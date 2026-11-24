# Changelog

All notable changes are documented here. Versions use numeric dotted identifiers and release tags use the same value prefixed with `v`.

## 2026.11.24.3

### Changed

- Exclude unused playback decoders and their native library dependencies from release builds with the `nodsp` build tag
- Statically link the Linux C++ runtime and validate release dependencies with `ldd`
- Remove native playback-library and C++ runtime version conflicts from Linux release executables

## 2026.11.24.2

### Changed

- Reuse authenticated sessions and HTTP clients for all tracks handled by the same batch worker
- Download video and audio profiles and their segments concurrently with bounded workers
- Preserve completed video segments for interrupted downloads and assemble them in order on resume
- Decrypt video and audio tracks concurrently and embed metadata and cover art during the initial mux
- Standardize first-party messages and tooling on English
- Curate release notes and add a terminal demo

### Added

- Unit coverage for video segment ordering, failed downloads, and segment reuse
- Local end-to-end coverage for WebVTT subtitle download and MP4 embedding
- Dependabot configuration for Go modules and GitHub Actions
- Dependency-only vulnerability checks with govulncheck

## 2026.11.24.1

### Added

- Windows x86-64 executable with the same audio, video, batch, resume, metadata, and self-update features as Linux
- Windows-safe output filenames and statically linked MinGW/C++ runtime
- Native Windows CI and GitHub Actions release packages for Linux and Windows

### Verified

- Windows engine and update tests under Wine
- Real FLAC, OGG, and 1080p MP4 downloads with full decode verification under Wine

## 2026.11.23.1

### Changed

- Raised the build requirement to Go 1.27 and updated application and test dependencies
- Updated CI and release automation to `actions/checkout` 7.0.1 and `actions/setup-go` 7.1.0

### Added

- Linux x86-64 CLI with embedded protocol and cipher resources for Spotify audio and music video downloads
- Best-quality FLAC, OGG Vorbis, and AAC selection with interactive format selection
- Audio-only, audio-plus-video, and video-only modes
- Album, artist, playlist, and link-list queues with bounded concurrency
- Duplicate detection, retries, and HTTP Range resume
- Metadata, cover art, lyrics, subtitles, and full output verification
- Reconstructed P-Code and nested bytecode engine with a Go CTR hot path
- Differential, concurrency, seek, partial-block, and shared-runtime reset tests
- HTTPS self-update with SHA-256 verification, self-testing, and atomic rollback
- CI and tagged-release workflows
- CC BY-NC-SA 4.0 licensing and collected third-party license texts

### Verified

- FLAC 24-bit downloads with complete decoding
- OGG and concurrent audio downloads
- 1920x1080 H.264/AAC video in `--video` and `--video-only` modes
- Self-update against a local release server, including replacement and rollback
- Release executable self-test and archive checksum validation

### Known limitations

- Current engine resources target Spotify 1.3.8.620 and Linux x86-64.
- Video requires a user-supplied Widevine L3 WVD file.
- WebVTT download and embedding are covered by a local end-to-end test; the tested Spotify tracks did not expose subtitle language codes.
