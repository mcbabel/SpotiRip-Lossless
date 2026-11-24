package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/spclient"
	widevine "github.com/iyear/gowidevine"
	widevinepb "github.com/iyear/gowidevine/widevinepb"
)

type videoEncryptionInfo struct {
	KeySystem             string `json:"key_system"`
	EncryptionScheme      string `json:"encryption_scheme"`
	LicenseServerEndpoint string `json:"license_server_endpoint"`
	EncryptionData        string `json:"encryption_data"`
}

type videoProfile struct {
	ID                int    `json:"id"`
	FileType          string `json:"file_type"`
	MimeType          string `json:"mime_type"`
	VideoCodec        string `json:"video_codec"`
	AudioCodec        string `json:"audio_codec"`
	MaxBitrate        int64  `json:"max_bitrate"`
	VideoBitrate      int64  `json:"video_bitrate"`
	AudioBitrate      int64  `json:"audio_bitrate"`
	VideoWidth        int    `json:"video_width"`
	VideoHeight       int    `json:"video_height"`
	EncryptionIndex   int    `json:"encryption_index"`
	EncryptionIndices []int  `json:"encryption_indices"`
}

type videoContent struct {
	SegmentLength   int64                 `json:"segment_length"`
	StartTimeMillis int64                 `json:"start_time_millis"`
	EndTimeMillis   int64                 `json:"end_time_millis"`
	Profiles        []videoProfile        `json:"profiles"`
	EncryptionInfos []videoEncryptionInfo `json:"encryption_infos"`
}

type videoManifest struct {
	Contents               []videoContent        `json:"contents"`
	EncryptionInfos        []videoEncryptionInfo `json:"encryption_infos"`
	BaseURLs               []string              `json:"base_urls"`
	InitializationTemplate string                `json:"initialization_template"`
	SegmentTemplate        string                `json:"segment_template"`
	SubtitleTemplate       string                `json:"subtitle_template"`
	SubtitleBaseURLs       []string              `json:"subtitle_base_urls"`
	SubtitleLanguageCodes  []string              `json:"subtitle_language_codes"`
	StartTimeMillis        int64                 `json:"start_time_millis"`
	EndTimeMillis          int64                 `json:"end_time_millis"`
}

func (m *videoManifest) content() (*videoContent, error) {
	if len(m.Contents) == 0 {
		return nil, fmt.Errorf("video manifest contains no media")
	}
	return &m.Contents[0], nil
}

func (m *videoManifest) encryption() (videoEncryptionInfo, error) {
	content, err := m.content()
	if err != nil {
		return videoEncryptionInfo{}, err
	}
	infos := content.EncryptionInfos
	if len(infos) == 0 {
		infos = m.EncryptionInfos
	}
	for _, info := range infos {
		if info.KeySystem == "widevine" && info.EncryptionScheme == "cenc/ctr" && info.LicenseServerEndpoint != "" && info.EncryptionData != "" {
			return info, nil
		}
	}
	return videoEncryptionInfo{}, fmt.Errorf("no Widevine CENC entry in video manifest")
}

func (m *videoManifest) durationMillis() (int64, error) {
	content, err := m.content()
	if err != nil {
		return 0, err
	}
	start := content.StartTimeMillis
	end := content.EndTimeMillis
	if end == 0 {
		start = m.StartTimeMillis
		end = m.EndTimeMillis
	}
	if end <= start {
		return 0, fmt.Errorf("invalid video duration")
	}
	return end - start, nil
}

func (m *videoManifest) selectProfiles(maxHeight int) (videoProfile, videoProfile, error) {
	content, err := m.content()
	if err != nil {
		return videoProfile{}, videoProfile{}, err
	}
	var video, audio videoProfile
	foundVideo, foundAudio := false, false
	for _, profile := range content.Profiles {
		if profile.FileType != "mp4" || !supportsWidevine(profile) {
			continue
		}
		switch profile.MimeType {
		case "video/mp4":
			if maxHeight > 0 && profile.VideoHeight > maxHeight {
				continue
			}
			if !foundVideo || profile.MaxBitrate > video.MaxBitrate {
				video = profile
				foundVideo = true
			}
		case "audio/mp4":
			if !foundAudio || profile.MaxBitrate > audio.MaxBitrate {
				audio = profile
				foundAudio = true
			}
		}
	}
	if !foundVideo {
		return video, audio, fmt.Errorf("no compatible MP4 video profile found")
	}
	if !foundAudio {
		return video, audio, fmt.Errorf("no compatible MP4 audio profile found")
	}
	return video, audio, nil
}

func supportsWidevine(profile videoProfile) bool {
	for _, index := range profile.EncryptionIndices {
		if index == 0 {
			return true
		}
	}
	return len(profile.EncryptionIndices) == 0 && profile.EncryptionIndex == 0
}

func acquireWidevineKeys(ctx context.Context, sp *spclient.Spclient, info videoEncryptionInfo, path string) ([]*widevine.Key, error) {
	if path == "" {
		return nil, fmt.Errorf("--wvd requires a Widevine device file")
	}
	deviceFile, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Widevine device file: %w", err)
	}
	device, err := widevine.NewDevice(widevine.FromWVD(deviceFile))
	closeErr := deviceFile.Close()
	if err != nil {
		return nil, fmt.Errorf("Widevine device file: %w", err)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	psshData, err := base64.StdEncoding.DecodeString(info.EncryptionData)
	if err != nil {
		return nil, fmt.Errorf("Widevine PSSH: %w", err)
	}
	pssh, err := widevine.NewPSSH(psshData)
	if err != nil {
		return nil, fmt.Errorf("Widevine PSSH: %w", err)
	}
	cdm := widevine.NewCDM(device)
	challenge, parseLicense, err := cdm.GetLicenseChallenge(pssh, widevinepb.LicenseType_AUTOMATIC, false)
	if err != nil {
		return nil, fmt.Errorf("Widevine challenge: %w", err)
	}
	response, err := sp.Request(ctx, http.MethodPost, info.LicenseServerEndpoint, nil, http.Header{
		"Content-Type": []string{"application/octet-stream"},
		"Origin":       []string{"https://xpui.app.spotify.com"},
		"Referer":      []string{"https://xpui.app.spotify.com/"},
	}, challenge)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Widevine license: HTTP %d", response.StatusCode)
	}
	keys, err := parseLicense(body)
	if err != nil {
		return nil, fmt.Errorf("Widevine license: %w", err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("Widevine license contains no content key")
	}
	return keys, nil
}

func manifestURL(base string, template string, profile videoProfile) string {
	value := strings.NewReplacer(
		"{{profile_id}}", strconv.Itoa(profile.ID),
		"{{file_type}}", profile.FileType,
	).Replace(template)
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(value, "/")
}

func fetchSegment(ctx context.Context, client *http.Client, manifest *videoManifest, template string, profile videoProfile, timestamp *int64, output io.Writer) error {
	var lastErr error
	for _, base := range manifest.BaseURLs {
		url := manifestURL(base, template, profile)
		if timestamp != nil {
			url = strings.Replace(url, "{{segment_timestamp}}", strconv.FormatInt(*timestamp, 10), 1)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		response, err := doWithRetry(ctx, client, request)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			lastErr = &httpStatusError{StatusCode: response.StatusCode}
			continue
		}
		var content bytes.Buffer
		_, err = io.Copy(&content, response.Body)
		response.Body.Close()
		if err == nil {
			_, err = output.Write(content.Bytes())
			if err == nil {
				return nil
			}
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no CDN address available")
	}
	return fmt.Errorf("could not download segment: %w", lastErr)
}

type segmentDownload struct {
	template  string
	timestamp *int64
	path      string
}

func downloadSegmentToFile(ctx context.Context, client *http.Client, manifest *videoManifest, profile videoProfile, item segmentDownload) error {
	if info, err := os.Stat(item.path); err == nil && info.Size() > 0 {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary := item.path + ".tmp"
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	if err := fetchSegment(ctx, client, manifest, item.template, profile, item.timestamp, output); err != nil {
		output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, item.path)
}

func assembleSegments(path string, items []segmentDownload) error {
	output, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, item := range items {
		input, err := os.Open(item.path)
		if err != nil {
			output.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := input.Close()
		if copyErr != nil {
			output.Close()
			return copyErr
		}
		if closeErr != nil {
			output.Close()
			return closeErr
		}
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func downloadProfile(ctx context.Context, client *http.Client, manifest *videoManifest, profile videoProfile, path string, progressMu *sync.Mutex) error {
	content, err := manifest.content()
	if err != nil {
		return err
	}
	if content.SegmentLength <= 0 {
		return fmt.Errorf("invalid segment length")
	}
	duration, err := manifest.durationMillis()
	if err != nil {
		return err
	}
	segmentDir := fmt.Sprintf("%s.%d-%d.segments", path, profile.ID, content.SegmentLength)
	if err := os.MkdirAll(segmentDir, 0o700); err != nil {
		return err
	}
	items := []segmentDownload{{
		template: manifest.InitializationTemplate,
		path:     filepath.Join(segmentDir, "init.seg"),
	}}
	stepMillis := content.SegmentLength * 1000
	count := (duration + stepMillis - 1) / stepMillis
	for index := int64(0); index < count; index++ {
		timestamp := index * content.SegmentLength
		items = append(items, segmentDownload{
			template:  manifest.SegmentTemplate,
			timestamp: &timestamp,
			path:      filepath.Join(segmentDir, fmt.Sprintf("%08d.seg", index)),
		})
	}
	jobs := make(chan segmentDownload, len(items))
	for _, item := range items {
		jobs <- item
	}
	close(jobs)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsCh := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(min(4, len(items)))
	completed := 0
	for range min(4, len(items)) {
		go func() {
			defer workers.Done()
			for item := range jobs {
				if workerCtx.Err() != nil {
					return
				}
				if err := downloadSegmentToFile(workerCtx, client, manifest, profile, item); err != nil {
					select {
					case errorsCh <- err:
					default:
					}
					cancel()
					return
				}
				progressMu.Lock()
				completed++
				if completed%10 == 0 || completed == len(items) {
					fmt.Printf("\r  %s %d%%", profile.MimeType, completed*100/len(items))
				}
				progressMu.Unlock()
			}
		}()
	}
	workers.Wait()
	select {
	case err := <-errorsCh:
		return err
	default:
	}
	progressMu.Lock()
	fmt.Println()
	progressMu.Unlock()
	if err := assembleSegments(path, items); err != nil {
		return err
	}
	return os.RemoveAll(segmentDir)
}

func decryptMP4(inputPath, outputPath string, keys []*widevine.Key) error {
	input, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := widevine.DecryptMP4Auto(input, keys, output); err != nil {
		output.Close()
		return fmt.Errorf("CENC decryption: %w", err)
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func runTwo(first, second func() error) error {
	results := make(chan error, 2)
	go func() { results <- first() }()
	go func() { results <- second() }()
	firstErr := <-results
	secondErr := <-results
	if firstErr != nil {
		return firstErr
	}
	return secondErr
}

func downloadVideo(ctx context.Context, sp *spclient.Spclient, client *http.Client, track *metadatapb.Track, options cliOptions) error {
	if len(track.OriginalVideo) == 0 || len(track.OriginalVideo[0].GetGid()) == 0 {
		return fmt.Errorf("no music video is available for this track")
	}
	manifestID := hex.EncodeToString(track.OriginalVideo[0].GetGid())
	response, err := sp.Request(ctx, http.MethodGet, "/manifests/v9/json/sources/"+manifestID+"/options/supports_drm", nil, http.Header{
		"Origin":  []string{"https://xpui.app.spotify.com"},
		"Referer": []string{"https://xpui.app.spotify.com/"},
		"Accept":  []string{"*/*"},
	}, nil)
	if err != nil {
		return err
	}
	manifestData, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("video manifest: HTTP %d", response.StatusCode)
	}
	var manifest videoManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("video manifest: %w", err)
	}
	video, audio, err := manifest.selectProfiles(options.videoQuality)
	if err != nil {
		return err
	}
	fmt.Printf("Video: %dx%d, %.1f Mbit/s; AAC: %.0f kbit/s\n", video.VideoWidth, video.VideoHeight, float64(video.MaxBitrate)/1e6, float64(audio.MaxBitrate)/1000)
	encryption, err := manifest.encryption()
	if err != nil {
		return err
	}
	keys, err := acquireWidevineKeys(ctx, sp, encryption, options.wvdPath)
	if err != nil {
		return err
	}
	outputPath, err := renderOutputPath(options, track, ".mp4")
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
	var subtitleFiles []subtitleFile
	if options.subtitles {
		subtitleFiles, err = downloadSubtitles(ctx, client, &manifest, outputPath)
		if err != nil {
			return err
		}
	}
	videoEncrypted := outputPath + ".video-encrypted.part"
	audioEncrypted := outputPath + ".audio-encrypted.part"
	videoDecrypted := outputPath + ".video-decrypted.part"
	audioDecrypted := outputPath + ".audio-decrypted.part"
	muxPart := outputPath + ".part"
	for _, path := range []string{videoEncrypted, audioEncrypted, videoDecrypted, audioDecrypted, muxPart} {
		defer os.Remove(path)
	}
	downloadClient := *client
	downloadClient.Timeout = 0
	fmt.Println("Downloading video and audio tracks ...")
	var progressMu sync.Mutex
	if err := runTwo(
		func() error {
			return downloadProfile(ctx, &downloadClient, &manifest, video, videoEncrypted, &progressMu)
		},
		func() error {
			return downloadProfile(ctx, &downloadClient, &manifest, audio, audioEncrypted, &progressMu)
		},
	); err != nil {
		return err
	}
	if err := runTwo(
		func() error { return decryptMP4(videoEncrypted, videoDecrypted, keys) },
		func() error { return decryptMP4(audioEncrypted, audioDecrypted, keys) },
	); err != nil {
		return err
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg is required to mux the video file: %w", err)
	}
	cover, err := downloadCover(ctx, client, track, options.outputDir)
	if err != nil {
		fmt.Printf("Warning: could not download cover: %v\n", err)
		cover = ""
	}
	if cover != "" {
		defer os.Remove(cover)
	}
	if err := muxVideo(ctx, ffmpeg, videoDecrypted, audioDecrypted, outputPath, subtitleFiles, cover, track, options.link); err != nil {
		return err
	}
	if options.verify {
		if err := verifyMedia(ctx, outputPath); err != nil {
			return err
		}
		fmt.Printf("Validated: %s\n", outputPath)
	}
	fmt.Printf("Video completed: %s\n", outputPath)
	return nil
}

func muxVideo(ctx context.Context, ffmpeg, videoDecrypted, audioDecrypted, outputPath string, subtitleFiles []subtitleFile, cover string, track *metadatapb.Track, link string) error {
	muxPart := outputPath + ".part"
	arguments := []string{
		"-y", "-loglevel", "error",
		"-i", videoDecrypted, "-i", audioDecrypted,
	}
	for _, file := range subtitleFiles {
		arguments = append(arguments, "-i", file.Path)
	}
	coverInput := 0
	if cover != "" {
		coverInput = len(subtitleFiles) + 2
		arguments = append(arguments, "-i", cover)
	}
	arguments = append(arguments,
		"-map", "0:v:0", "-map", "1:a:0", "-map_metadata", "-1", "-c", "copy",
	)
	for i, file := range subtitleFiles {
		arguments = append(arguments,
			"-map", fmt.Sprintf("%d:s:0", i+2),
			"-c:s", "mov_text",
			fmt.Sprintf("-metadata:s:s:%d", i), "language="+file.Language,
		)
	}
	if cover != "" {
		arguments = append(arguments,
			"-map", strconv.Itoa(coverInput),
			"-disposition:v:1", "attached_pic", "-metadata:s:v:1", "title=Album cover",
		)
	}
	arguments = append(arguments, metadataArguments(track, link)...)
	arguments = append(arguments, "-movflags", "+faststart+use_metadata_tags", "-f", "mp4", muxPart)
	command := exec.CommandContext(ctx, ffmpeg, arguments...)
	var commandOutput bytes.Buffer
	command.Stdout = &commandOutput
	command.Stderr = &commandOutput
	if err := command.Run(); err != nil {
		return fmt.Errorf("MP4 muxing: %w: %s", err, strings.TrimSpace(commandOutput.String()))
	}
	return os.Rename(muxPart, outputPath)
}
