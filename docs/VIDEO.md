# Music video downloads

## Usage

Download audio and music video:

```bash
./spotirip-lossless --video --wvd "/path/to/device.wvd" "SPOTIFY_TRACK_URL"
```

The audio-only file is retained, and an additional MP4 is created. Use `--video-only` to skip the separate audio-only download. `--video-quality 720` limits the video height, `--subtitles` processes available subtitle languages, and `--verify` fully validates the finished MP4.

## Requirements

- `ffmpeg` in `PATH` for muxing and MP4 metadata
- A user-supplied Widevine L3 WVD file for license processing
- A track with `original_video` metadata and an accessible video manifest

The WVD file is neither embedded nor logged. It is only used for Widevine license processing.

## Manifest

The current path uses:

```text
/manifests/v9/json/sources/<manifest-id>/options/supports_drm
```

The manifest ID comes from `Track.OriginalVideo`. The CLI reads:

- profiles, containers, codecs, bitrates, and resolutions,
- initialization and segment templates,
- segment duration and video duration,
- Widevine `encryption_data` and the license endpoint,
- available CDN base URLs,
- subtitle template, subtitle base URLs, and language codes.

## Profile selection

Only compatible MP4 profiles are selected. Video and audio are each chosen by the highest offered bitrate. `--video-quality` first rejects profiles above the requested height. If the exact height is unavailable, the next lower profile is selected. For example, a limit of 360 selected a 568x320 profile in testing. Without a limit, the verified example selected 1920x1080 H.264 video and stereo AAC audio.

## Widevine and CENC

Widevine `encryption_data` contains the PSSH information. `gowidevine` builds the license challenge, and the authenticated Spotify session sends it to the manifest's Widevine endpoint.

The license response supplies the content key. Initialization and media segments are decrypted locally with CENC. The WVD private key is not sent to the server.

## Segment addressing

Segments are addressed by absolute time. With a four-second segment duration, timestamps are `0`, `4`, `8`, and so on. The initialization segment is written first, followed by media segments in order.

Available CDN base URLs are tried after a failure. Partial data from different responses is never mixed into one segment.

## Muxing and metadata

After CENC decryption, FFmpeg combines video and audio without re-encoding:

- H.264 video and AAC audio are preserved,
- title, artists, album, date, ISRC, track/disc numbers, Spotify URL, and available cover art are added.

The cover is added as a separate stream with `attached_pic`. The actual video stream already exists, so the cover is not assumed to be video stream 0. FFprobe determines the number of existing video streams independently of audio and subtitle streams.

## Subtitles

With `--subtitles`, the CLI substitutes the language placeholder in the subtitle template and tries all offered base URLs. Each language is downloaded completely, stored as `.vtt` next to the MP4, and embedded as `mov_text` with its language code. Temporary files prevent incomplete subtitles from appearing complete.

If the manifest contains no language codes, no subtitle files are created and the media streams remain unchanged. WebVTT download, language assignment, and `mov_text` embedding are covered by a local end-to-end test. The tested Spotify tracks did not expose subtitle language codes, so subtitle availability against Spotify remains catalog-dependent.

## Limitations

The current implementation selects MP4/H.264/AAC with Widevine CENC. It does not support arbitrary video formats. Missing videos, missing WVD files, and incompatible manifests produce explicit errors.
