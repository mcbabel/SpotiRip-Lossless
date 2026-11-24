package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	audiofilespb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata/audiofiles"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	streamingpb "github.com/devgianlu/go-librespot/proto/spotify/streaming"
	"github.com/devgianlu/go-librespot/session"
	"github.com/mcbabel/SpotiRip-Lossless/internal/spotifyclean"
)

var trackURLPattern = regexp.MustCompile(`(?:spotify:track:|open\.spotify\.com/(?:intl-[\w-]+/)?track/)([A-Za-z0-9]{22})`)

type credentials struct {
	Username string `json:"username"`
	Data     []byte `json:"data"`
	DeviceID string `json:"device_id"`
}

type cliOptions struct {
	outputDir      string
	credentialPath string
	link           string
	selectFormat   bool
	video          bool
	wvdPath        string
	help           bool
	concurrent     int
	limit          int
	onDuplicate    string
	audioQuality   string
	videoQuality   int
	videoOnly      bool
	outputTemplate string
	lyrics         bool
	subtitles      bool
	verify         bool
	selfTest       bool
	selfUpdate     bool
	showVersion    bool
	updateManifest string
}

type ctrWriter struct {
	writer io.Writer
	engine *spotifyclean.Engine
	offset uint64
}

func (w *ctrWriter) Write(p []byte) (int, error) {
	if err := w.engine.Decrypt(w.offset, p); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(p)
	w.offset += uint64(n)
	return n, err
}

func loadCredentials(path string) (*credentials, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		deviceID := make([]byte, 20)
		if _, err := rand.Read(deviceID); err != nil {
			return nil, false, err
		}
		return &credentials{DeviceID: hex.EncodeToString(deviceID)}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var stored credentials
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, false, fmt.Errorf("invalid credential file: %w", err)
	}
	return &stored, true, nil
}

func (c *credentials) save(path string) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func parseTrack(value string) (*librespot.SpotifyId, error) {
	match := trackURLPattern.FindStringSubmatch(value)
	if match == nil {
		return nil, fmt.Errorf("a Spotify track link is required")
	}
	id, err := librespot.SpotifyIdFromBase62(librespot.SpotifyIdTypeTrack, match[1])
	if err != nil {
		return nil, err
	}
	return id, nil
}

func audioQuality(entry *audiofilespb.ExtendedAudioFile) int64 {
	quality := int64(entry.AverageBitrate)
	switch entry.File.GetFormat() {
	case metadatapb.AudioFile_FLAC_FLAC_24BIT:
		return 1<<62 + 1
	case metadatapb.AudioFile_FLAC_FLAC:
		return 1 << 62
	default:
		return quality
	}
}

func selectAudio(files []*audiofilespb.ExtendedAudioFile) *audiofilespb.ExtendedAudioFile {
	var selected *audiofilespb.ExtendedAudioFile
	for _, entry := range files {
		if entry == nil || entry.File == nil || entry.File.Format == nil {
			continue
		}
		if selected == nil || audioQuality(entry) > audioQuality(selected) {
			selected = entry
		}
	}
	return selected
}

func audioDescription(entry *audiofilespb.ExtendedAudioFile) string {
	var name string
	switch entry.File.GetFormat() {
	case metadatapb.AudioFile_FLAC_FLAC_24BIT:
		name = "FLAC 24-bit (verlustfrei)"
	case metadatapb.AudioFile_FLAC_FLAC:
		name = "FLAC (verlustfrei)"
	case metadatapb.AudioFile_OGG_VORBIS_96, metadatapb.AudioFile_OGG_VORBIS_160, metadatapb.AudioFile_OGG_VORBIS_320:
		name = "Ogg Vorbis"
	case metadatapb.AudioFile_MP3_256, metadatapb.AudioFile_MP3_320, metadatapb.AudioFile_MP3_160, metadatapb.AudioFile_MP3_96, metadatapb.AudioFile_MP3_160_ENC:
		name = "MP3"
	case metadatapb.AudioFile_AAC_24, metadatapb.AudioFile_AAC_48:
		name = "AAC"
	case metadatapb.AudioFile_XHE_AAC_24, metadatapb.AudioFile_XHE_AAC_16, metadatapb.AudioFile_XHE_AAC_12:
		name = "xHE-AAC"
	default:
		name = entry.File.GetFormat().String()
	}
	if entry.AverageBitrate > 0 {
		return fmt.Sprintf("%s, ca. %.0f kbit/s", name, float64(entry.AverageBitrate)/1000)
	}
	return name
}

func matchesAudioQuality(format metadatapb.AudioFile_Format, quality string) bool {
	switch quality {
	case "", "best":
		return true
	case "flac":
		return format == metadatapb.AudioFile_FLAC_FLAC || format == metadatapb.AudioFile_FLAC_FLAC_24BIT
	case "ogg320":
		return format == metadatapb.AudioFile_OGG_VORBIS_320
	case "ogg160":
		return format == metadatapb.AudioFile_OGG_VORBIS_160
	case "aac":
		switch format {
		case metadatapb.AudioFile_AAC_24, metadatapb.AudioFile_AAC_48, metadatapb.AudioFile_XHE_AAC_24, metadatapb.AudioFile_XHE_AAC_16, metadatapb.AudioFile_XHE_AAC_12:
			return true
		}
	}
	return false
}

func chooseAudio(files []*audiofilespb.ExtendedAudioFile, interactive bool, quality string) (*audiofilespb.ExtendedAudioFile, error) {
	available := make([]*audiofilespb.ExtendedAudioFile, 0, len(files))
	for _, entry := range files {
		if entry != nil && entry.File != nil && entry.File.Format != nil && matchesAudioQuality(entry.File.GetFormat(), quality) {
			available = append(available, entry)
		}
	}
	if len(available) == 0 {
		return nil, fmt.Errorf("no audio file is available for this track")
	}
	sort.SliceStable(available, func(i, j int) bool {
		return audioQuality(available[i]) > audioQuality(available[j])
	})
	if !interactive {
		return available[0], nil
	}
	fmt.Println("Available audio formats:")
	for i, entry := range available {
		fmt.Printf("  %d) %s\n", i+1, audioDescription(entry))
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Selection [1 = highest quality]: ")
		input, err := reader.ReadString('\n')
		if err != nil && len(input) == 0 {
			return available[0], nil
		}
		input = strings.TrimSpace(input)
		if input == "" {
			return available[0], nil
		}
		choice, parseErr := strconv.Atoi(input)
		if parseErr == nil && choice >= 1 && choice <= len(available) {
			return available[choice-1], nil
		}
		fmt.Printf("Enter a number from 1 to %d.\n", len(available))
	}
}

func audioExtension(format metadatapb.AudioFile_Format) string {
	switch format {
	case metadatapb.AudioFile_FLAC_FLAC, metadatapb.AudioFile_FLAC_FLAC_24BIT:
		return ".flac"
	case metadatapb.AudioFile_OGG_VORBIS_96, metadatapb.AudioFile_OGG_VORBIS_160, metadatapb.AudioFile_OGG_VORBIS_320:
		return ".ogg"
	case metadatapb.AudioFile_MP3_256, metadatapb.AudioFile_MP3_320, metadatapb.AudioFile_MP3_160, metadatapb.AudioFile_MP3_96, metadatapb.AudioFile_MP3_160_ENC:
		return ".mp3"
	case metadatapb.AudioFile_AAC_24, metadatapb.AudioFile_AAC_48, metadatapb.AudioFile_XHE_AAC_24, metadatapb.AudioFile_XHE_AAC_16, metadatapb.AudioFile_XHE_AAC_12:
		return ".m4a"
	default:
		return ".audio"
	}
}

func safeName(value string) string {
	return safeNameForOS(value, runtime.GOOS)
}

func safeNameForOS(value, goos string) string {
	value = regexp.MustCompile(`[\x00-\x1f/\\]+`).ReplaceAllString(value, "_")
	if goos == "windows" {
		value = regexp.MustCompile(`[<>:"|?*]+`).ReplaceAllString(value, "_")
	}
	value = strings.Trim(strings.TrimSpace(value), ".")
	if value == "" {
		return "Unknown"
	}
	if goos == "windows" {
		base := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
		reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CLOCK$"
		if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			reserved = true
		}
		if reserved {
			value = "_" + value
		}
	}
	if len(value) > 160 {
		value = value[:160]
	}
	return value
}

func download(ctx context.Context, options cliOptions, sess *session.Session, client *http.Client) error {
	trackID, err := parseTrack(options.link)
	if err != nil {
		return err
	}
	outputDir := options.outputDir
	sp := sess.Spclient()
	var track metadatapb.Track
	if err := sp.ExtendedMetadataSimple(ctx, *trackID, extmetadatapb.ExtensionKind_TRACK_V4, &track); err != nil {
		return fmt.Errorf("track metadata: %w", err)
	}
	actualID := librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, track.Gid)
	options.link = canonicalTrack(actualID.Uri())
	if err := ensureCoverMetadata(ctx, sp, &track); err != nil {
		fmt.Printf("Warning: %v\n", err)
	}
	if options.videoOnly {
		return downloadVideo(ctx, sp, client, &track, options)
	}
	var audioFiles audiofilespb.AudioFilesExtensionResponse
	if err := sp.ExtendedMetadataSimple(ctx, actualID, extmetadatapb.ExtensionKind_AUDIO_FILES, &audioFiles); err != nil {
		return fmt.Errorf("audio formats: %w", err)
	}
	selected, err := chooseAudio(audioFiles.Files, options.selectFormat, options.audioQuality)
	if err != nil {
		return err
	}
	file := selected.File
	fmt.Printf("Audio file %x (%s, %d bit/s)\n", file.FileId, file.Format.String(), selected.AverageBitrate)

	metadata, err := spotifyclean.ReadClientMetadata()
	if err != nil {
		return fmt.Errorf("client metadata: %w", err)
	}
	playPlayToken, err := hex.DecodeString(metadata.PlayPlayToken)
	if err != nil {
		return fmt.Errorf("PlayPlay token: %w", err)
	}
	sp.SetAppVersion(metadata.AppVersion)
	license, err := sp.PlayPlayRequest(ctx, file.FileId, &streamingpb.PlayPlayLicenseRequest{
		Version:       ptr(metadata.PlayPlayVersion),
		Token:         playPlayToken,
		ContentType:   ptr(streamingpb.ContentType_AUDIO_TRACK),
		Interactivity: ptr(streamingpb.Interactivity(3)),
		Timestamp:     ptr(time.Now().Unix()),
	})
	if err != nil {
		return fmt.Errorf("PlayPlay license: %w", err)
	}
	engine, err := spotifyclean.New()
	if err != nil {
		return err
	}
	defer engine.Close()
	key, err := engine.Derive(license.RawResponse)
	if err != nil {
		return err
	}
	if err := engine.InitCipher(key); err != nil {
		return err
	}

	resolved, err := sp.ResolveStorageInteractive(ctx, file.FileId, file.Format, false)
	if err != nil {
		return fmt.Errorf("Storage Resolve: %w", err)
	}
	if len(resolved.Cdnurl) == 0 {
		return fmt.Errorf("Spotify returned no CDN URL")
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	outputPath, err := renderOutputPath(options, &track, audioExtension(file.GetFormat()))
	if err != nil {
		return err
	}
	outputPath, err = prepareOutputPath(outputPath, options.onDuplicate)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}
	if err != nil {
		return err
	}
	partPath := outputPath + ".part"
	out, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("output file: %w", err)
	}
	defer out.Close()
	resumeOffset, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("resume position: %w", err)
	}

	downloadClient := *client
	downloadClient.Timeout = 0
	var lastErr error
	refreshCount := 0
	for attempt := 0; attempt < len(resolved.Cdnurl); attempt++ {
		cdnURL := resolved.Cdnurl[attempt]
		if _, err := out.Seek(resumeOffset, io.SeekStart); err != nil {
			return err
		}
		if err := out.Truncate(resumeOffset); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		if resumeOffset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeOffset))
		}
		resp, err := doWithRetry(ctx, &downloadClient, req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && resumeOffset > 0 {
			resp.Body.Close()
			resumeOffset = 0
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := out.Truncate(0); err != nil {
				return err
			}
			attempt--
			continue
		}
		if resp.StatusCode == http.StatusForbidden && refreshCount == 0 {
			resp.Body.Close()
			refreshCount++
			resolved, err = sp.ResolveStorageInteractive(ctx, file.FileId, file.Format, false)
			if err != nil {
				lastErr = err
				continue
			}
			if len(resolved.Cdnurl) == 0 {
				lastErr = fmt.Errorf("the renewed storage resolve response contains no CDN URL")
				break
			}
			attempt = -1
			continue
		}
		if resp.StatusCode != http.StatusOK && !(resumeOffset > 0 && resp.StatusCode == http.StatusPartialContent) {
			resp.Body.Close()
			lastErr = fmt.Errorf("CDN HTTP %d", resp.StatusCode)
			continue
		}
		if resumeOffset > 0 && resp.StatusCode == http.StatusPartialContent {
			var start int64
			if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-", &start); err != nil || start != resumeOffset {
				resp.Body.Close()
				resumeOffset = 0
				if _, err := out.Seek(0, io.SeekStart); err != nil {
					return err
				}
				if err := out.Truncate(0); err != nil {
					return err
				}
				attempt--
				continue
			}
		}
		if resumeOffset > 0 && resp.StatusCode == http.StatusOK {
			resumeOffset = 0
			if _, err := out.Seek(0, 0); err != nil {
				return err
			}
			if err := out.Truncate(0); err != nil {
				return err
			}
		}
		writer := &ctrWriter{writer: out, engine: engine, offset: uint64(resumeOffset)}
		_, err = io.CopyBuffer(writer, resp.Body, make([]byte, 256*1024))
		resp.Body.Close()
		if err == nil {
			if err := out.Sync(); err != nil {
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			if err := validateAudio(partPath, file.GetFormat()); err != nil {
				return err
			}
			if err := normalizeAudio(partPath, file.GetFormat()); err != nil {
				return err
			}
			if err := os.Rename(partPath, outputPath); err != nil {
				return err
			}
			fmt.Printf("Audio completed: %s\n", outputPath)
			if err := embedMetadata(ctx, client, &track, options.link, outputPath, outputDir); err != nil {
				fmt.Printf("Warning: could not embed metadata/cover: %v\n", err)
			}
			if options.lyrics {
				lyricsPath, err := downloadLyrics(ctx, client, &track, outputPath)
				if err != nil {
					fmt.Printf("Warning: could not download lyrics: %v\n", err)
				} else if lyricsPath != "" {
					fmt.Printf("Lyrics completed: %s\n", lyricsPath)
				} else {
					fmt.Println("No lyrics found on LRCLIB")
				}
			}
			if options.verify {
				if err := verifyMedia(ctx, outputPath); err != nil {
					return err
				}
				fmt.Printf("Validated: %s\n", outputPath)
			}
			if options.video {
				if err := downloadVideo(ctx, sp, client, &track, options); err != nil {
					return fmt.Errorf("video download: %w", err)
				}
			}
			return nil
		}
		lastErr = err
		resumeOffset, _ = out.Seek(0, io.SeekEnd)
	}
	return fmt.Errorf("CDN download: %w", lastErr)
}

func validateAudio(path string, format metadatapb.AudioFile_Format) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil {
		return fmt.Errorf("audio header: %w", err)
	}
	valid := true
	switch format {
	case metadatapb.AudioFile_FLAC_FLAC, metadatapb.AudioFile_FLAC_FLAC_24BIT:
		valid = string(header[:4]) == "fLaC"
	case metadatapb.AudioFile_OGG_VORBIS_96, metadatapb.AudioFile_OGG_VORBIS_160, metadatapb.AudioFile_OGG_VORBIS_320:
		valid = string(header[:4]) == "OggS"
	case metadatapb.AudioFile_MP3_256, metadatapb.AudioFile_MP3_320, metadatapb.AudioFile_MP3_160, metadatapb.AudioFile_MP3_96, metadatapb.AudioFile_MP3_160_ENC:
		valid = string(header[:3]) == "ID3" || header[0] == 0xff && header[1]&0xe0 == 0xe0
	case metadatapb.AudioFile_AAC_24, metadatapb.AudioFile_AAC_48, metadatapb.AudioFile_XHE_AAC_24, metadatapb.AudioFile_XHE_AAC_16, metadatapb.AudioFile_XHE_AAC_12:
		valid = string(header[4:8]) == "ftyp" || header[0] == 0xff && header[1]&0xf0 == 0xf0
	}
	if !valid {
		return fmt.Errorf("the decoded file does not match format %s", format.String())
	}
	return nil
}

func normalizeAudio(path string, format metadatapb.AudioFile_Format) error {
	switch format {
	case metadatapb.AudioFile_OGG_VORBIS_96, metadatapb.AudioFile_OGG_VORBIS_160, metadatapb.AudioFile_OGG_VORBIS_320:
	default:
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for offset := 0; offset+27 <= len(data); {
		if string(data[offset:offset+4]) != "OggS" {
			return fmt.Errorf("invalid OGG page at byte %d", offset)
		}
		segments := int(data[offset+26])
		headerSize := 27 + segments
		if offset+headerSize > len(data) {
			return fmt.Errorf("truncated OGG page")
		}
		payload := offset + headerSize
		if payload+7 <= len(data) && data[payload] == 1 && string(data[payload+1:payload+7]) == "vorbis" {
			if offset == 0 {
				return nil
			}
			return os.WriteFile(path, data[offset:], 0o644)
		}
		pageSize := headerSize
		for _, size := range data[offset+27 : offset+headerSize] {
			pageSize += int(size)
		}
		if pageSize <= 0 || offset+pageSize > len(data) {
			return fmt.Errorf("invalid OGG page size")
		}
		offset += pageSize
	}
	return fmt.Errorf("no Vorbis header found in OGG file")
}

func ptr[T any](value T) *T { return &value }

func parseArguments(args []string) (cliOptions, error) {
	options := cliOptions{outputDir: "downloads", credentialPath: ".spotify-credentials.json", concurrent: 2, onDuplicate: "skip", audioQuality: "best"}
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if positionalOnly {
			if options.link != "" {
				return options, fmt.Errorf("only one Spotify link is allowed")
			}
			options.link = argument
			continue
		}
		if argument == "--" {
			positionalOnly = true
			continue
		}
		var value string
		switch {
		case argument == "-h" || argument == "--help":
			options.help = true
		case argument == "--select":
			options.selectFormat = true
		case argument == "--video":
			options.video = true
		case argument == "--video-only":
			options.video = true
			options.videoOnly = true
		case argument == "--lyrics":
			options.lyrics = true
		case argument == "--subtitles":
			options.subtitles = true
		case argument == "--verify":
			options.verify = true
		case argument == "--self-test":
			options.selfTest = true
		case argument == "--self-update" || argument == "--update":
			options.selfUpdate = true
		case argument == "--version":
			options.showVersion = true
		case argument == "--update-manifest":
			if i+1 >= len(args) {
				return options, fmt.Errorf("--update-manifest requires a URL")
			}
			i++
			options.updateManifest = args[i]
		case argument == "--concurrent" || argument == "--limit" || argument == "--video-quality":
			if i+1 >= len(args) {
				return options, fmt.Errorf("%s requires a value", argument)
			}
			i++
			value = args[i]
			number, err := strconv.Atoi(value)
			if err != nil || number <= 0 {
				return options, fmt.Errorf("%s expects a positive integer", argument)
			}
			switch argument {
			case "--concurrent":
				options.concurrent = number
			case "--limit":
				options.limit = number
			case "--video-quality":
				options.videoQuality = number
			}
		case argument == "--on-duplicate":
			if i+1 >= len(args) {
				return options, fmt.Errorf("--on-duplicate requires skip, replace, or new")
			}
			i++
			value = args[i]
			if value != "skip" && value != "replace" && value != "new" {
				return options, fmt.Errorf("--on-duplicate expects skip, replace, or new")
			}
			options.onDuplicate = value
		case argument == "--audio-quality":
			if i+1 >= len(args) {
				return options, fmt.Errorf("--audio-quality requires a value")
			}
			i++
			value = args[i]
			switch value {
			case "best", "flac", "ogg320", "ogg160", "aac":
				options.audioQuality = value
			default:
				return options, fmt.Errorf("unknown audio quality: %s", value)
			}
		case argument == "--output-template":
			if i+1 >= len(args) {
				return options, fmt.Errorf("--output-template requires a value")
			}
			i++
			options.outputTemplate = args[i]
		case argument == "-o" || argument == "--output" || argument == "-credentials" || argument == "--credentials" || argument == "--wvd":
			if i+1 >= len(args) {
				return options, fmt.Errorf("%s requires a value", argument)
			}
			i++
			value = args[i]
			if argument == "-o" || argument == "--output" {
				options.outputDir = value
			} else if argument == "--wvd" {
				options.wvdPath = value
			} else {
				options.credentialPath = value
			}
		case strings.HasPrefix(argument, "-o="):
			options.outputDir = strings.TrimPrefix(argument, "-o=")
		case strings.HasPrefix(argument, "--output="):
			options.outputDir = strings.TrimPrefix(argument, "--output=")
		case strings.HasPrefix(argument, "-credentials="):
			options.credentialPath = strings.TrimPrefix(argument, "-credentials=")
		case strings.HasPrefix(argument, "--credentials="):
			options.credentialPath = strings.TrimPrefix(argument, "--credentials=")
		case strings.HasPrefix(argument, "--wvd="):
			options.wvdPath = strings.TrimPrefix(argument, "--wvd=")
		case strings.HasPrefix(argument, "--update-manifest="):
			options.updateManifest = strings.TrimPrefix(argument, "--update-manifest=")
		case strings.HasPrefix(argument, "-"):
			return options, fmt.Errorf("unbekannter Parameter: %s", argument)
		default:
			if options.link != "" {
				return options, fmt.Errorf("only one Spotify link is allowed")
			}
			options.link = argument
		}
	}
	if options.selfTest && options.link == "" {
		options.link = "https://open.spotify.com/track/5HNCy40Ni5BZJFw1TKzRsC"
	}
	if !options.help && !options.selfUpdate && !options.showVersion && options.link == "" {
		return options, fmt.Errorf("Spotify link is required")
	}
	return options, nil
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "Usage: spotirip-lossless [-o downloads] [--select] [--video] <Spotify-Song-Link>")
	fmt.Fprintln(writer, "  --select       Interactively select the audio format")
	fmt.Fprintln(writer, "  --video        Also download the available music video")
	fmt.Fprintln(writer, "  --video-only   Download only the music video")
	fmt.Fprintln(writer, "  --wvd          Widevine device file for --video")
	fmt.Fprintln(writer, "  -o, --output   Output directory")
	fmt.Fprintln(writer, "  --credentials  Credential file path")
	fmt.Fprintln(writer, "  --concurrent   1-3 concurrent downloads (default: 2)")
	fmt.Fprintln(writer, "  --limit        Maximum number of tracks")
	fmt.Fprintln(writer, "  --on-duplicate skip, replace, or new (default: skip)")
	fmt.Fprintln(writer, "  --audio-quality best, flac, ogg320, ogg160, or aac")
	fmt.Fprintln(writer, "  --video-quality Maximum height in pixels")
	fmt.Fprintln(writer, "  --output-template {artist}/{album}/{track} - {title}.{ext}")
	fmt.Fprintln(writer, "  --lyrics       Download lyrics from LRCLIB")
	fmt.Fprintln(writer, "  --subtitles    Download and embed available video subtitles")
	fmt.Fprintln(writer, "  --verify       Fully decode downloaded files for verification")
	fmt.Fprintln(writer, "  --self-test    Verify login, metadata, PlayPlay, and cipher")
	fmt.Fprintln(writer, "  --self-update  Update the program from an update manifest")
	fmt.Fprintln(writer, "  --update-manifest URL  Update manifest URL")
	fmt.Fprintln(writer, "  --version      Show the program version")
}

func main() {
	options, err := parseArguments(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if options.help {
		printUsage(os.Stdout)
		return
	}
	if options.showVersion {
		fmt.Println(currentVersion)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if options.selfUpdate {
		if _, err := runSelfUpdate(ctx, options); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(ctx, options); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
