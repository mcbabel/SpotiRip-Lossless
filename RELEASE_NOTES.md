# SpotiRip Lossless 2026.11.24.3

SpotiRip Lossless is a command-line downloader for Spotify audio and music videos on Linux and Windows, using your own Spotify Premium account.

## Highlights

- High-quality FLAC and OGG audio downloads without audio re-encoding
- MP4 music video downloads with H.264 video and AAC audio
- Audio-only, audio-plus-video, and video-only modes
- Faster batch downloads by reusing each worker's authenticated session
- Parallel video/audio segment downloads with bounded concurrency
- Resume interrupted video downloads using completed segments
- Parallel video/audio decryption and single-pass metadata and cover embedding
- Albums, artists, playlists, link lists, metadata, cover art, retries, and HTTP Range audio resume
- Optional lyrics, subtitles, and full output verification with FFprobe and FFmpeg
- SHA-256-verified self-updates with an integrated self-test and rollback
- Portable Linux release without native playback-library or C++ runtime version conflicts

## Requirements

- Linux x86-64 or Windows x86-64
- A Spotify Premium account
- FFmpeg and FFprobe on `PATH` for video processing, most metadata operations, duplicate detection, and `--verify`
- A user-supplied Widevine L3 WVD device file for music videos
- No additional native playback or C++ runtime libraries for release executables

## Installation

Download the archive for your platform from this release. Extract it and run the executable in the extracted directory.

### Linux

```bash
tar -xzf spotirip-lossless-linux-amd64.tar.gz
cd spotirip-lossless-linux-amd64
./spotirip-lossless --version
```

### Windows CMD

```bat
tar -xzf spotirip-lossless-windows-amd64.tar.gz
cd spotirip-lossless-windows-amd64
spotirip-lossless.exe --version
```

On first use, the program displays a Spotify verification URL and one-time code. Sign in with your own Spotify account and approve access to start downloading.

## Documentation

The archive contains the full README, metadata and video guides, license notices, and a terminal demo. The online README also includes the demo and quick-start instructions.

## Verification

This version is validated with unit, integration, race, and native Windows tests, release builds, and real FLAC, OGG, and 1080p MP4 downloads. Video interruption and segment-resume behavior is also tested. Vulnerability checks are run for dependency changes.

## Known limitations

- Engine resources currently target Spotify 1.3.8.620 on Linux x86-64.
- Video downloads require a user-supplied Widevine L3 WVD device file.
- Real media downloads are tested on Linux and under Wine; native Windows tests run in GitHub Actions.
- WebVTT download and embedding are covered by a local test; subtitle availability depends on the languages exposed by the Spotify video manifest.

## License

First-party code, documentation, and generated semantic resources are source-available under CC BY-NC-SA 4.0. Third-party components retain their respective licenses. This license covers the software, not Spotify content.
