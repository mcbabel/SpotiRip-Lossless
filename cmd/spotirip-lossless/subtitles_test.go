package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

func TestSubtitleDownloadAndMux(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/en.vtt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/vtt")
		_, _ = fmt.Fprint(w, "WEBVTT\n\n00:00.000 --> 00:01.000\nSubtitle test\n")
	}))
	defer server.Close()
	outputPath := filepath.Join(t.TempDir(), "video.mp4")
	manifest := &videoManifest{
		SubtitleTemplate:      "{{language_code}}.vtt",
		SubtitleBaseURLs:      []string{server.URL},
		SubtitleLanguageCodes: []string{"en"},
	}
	subtitles, err := downloadSubtitles(ctx, server.Client(), manifest, outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(subtitles) != 1 || subtitles[0].Language != "en" {
		t.Fatalf("unexpected subtitles: %+v", subtitles)
	}
	data, err := os.ReadFile(subtitles[0].Path)
	if err != nil || !strings.Contains(string(data), "Subtitle test") {
		t.Fatalf("invalid subtitle file: %q, %v", data, err)
	}
	videoPath := filepath.Join(t.TempDir(), "video-input.mp4")
	audioPath := filepath.Join(t.TempDir(), "audio-input.mp4")
	for _, command := range [][]string{
		{"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5", "-t", "1", "-c:v", "mpeg4", videoPath},
		{"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=44100", "-t", "1", "-c:a", "aac", audioPath},
	} {
		output, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-y", "-loglevel", "error"}, command...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("create test media: %v: %s", err, output)
		}
	}
	track := &metadatapb.Track{Name: ptr("Subtitle test")}
	if err := muxVideo(ctx, ffmpeg, videoPath, audioPath, outputPath, subtitles, "", track, "spotify:track:test"); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "json", outputPath).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect output: %v: %s", err, output)
	}
	text := string(output)
	for _, expected := range []string{`"codec_name": "mpeg4"`, `"codec_name": "aac"`, `"codec_type": "subtitle"`, `"codec_name": "mov_text"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %s in %s", expected, text)
		}
	}
}
