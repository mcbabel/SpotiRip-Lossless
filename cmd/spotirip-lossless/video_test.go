package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDownloadProfileResumesSegments(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	counts := make(map[string]int)
	var countsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		countsMu.Lock()
		counts[path]++
		countsMu.Unlock()
		if path == "/seg-mp4-7-1" && fail.Load() {
			http.Error(writer, "temporary failure", http.StatusInternalServerError)
			return
		}
		switch path {
		case "/init-mp4-7":
			_, _ = writer.Write([]byte("I"))
		case "/seg-mp4-7-0":
			_, _ = writer.Write([]byte("0"))
		case "/seg-mp4-7-1":
			_, _ = writer.Write([]byte("1"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manifest := videoManifest{
		Contents:               []videoContent{{SegmentLength: 1, StartTimeMillis: 0, EndTimeMillis: 2000}},
		BaseURLs:               []string{server.URL},
		InitializationTemplate: "init-{{file_type}}-{{profile_id}}",
		SegmentTemplate:        "seg-{{file_type}}-{{profile_id}}-{{segment_timestamp}}",
	}
	profile := videoProfile{ID: 7, FileType: "mp4", MimeType: "video/mp4"}
	output := filepath.Join(t.TempDir(), "video.part")
	run := func() error {
		return downloadProfile(context.Background(), server.Client(), &manifest, profile, output, &sync.Mutex{})
	}
	if err := run(); err == nil {
		t.Fatal("expected the first download to fail")
	}
	segmentDir := fmt.Sprintf("%s.%d-%d.segments", output, profile.ID, 1)
	for _, name := range []string{"init.seg", "00000000.seg"} {
		info, err := os.Stat(filepath.Join(segmentDir, name))
		if err != nil || info.Size() == 0 {
			t.Fatalf("completed segment %s was not retained: %v", name, err)
		}
	}
	fail.Store(false)
	if err := run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "I01" {
		t.Fatalf("segments assembled out of order: %q", data)
	}
	countsMu.Lock()
	defer countsMu.Unlock()
	if counts["/init-mp4-7"] != 1 || counts["/seg-mp4-7-0"] != 1 {
		t.Fatalf("completed segments were downloaded again: %+v", counts)
	}
	if counts["/seg-mp4-7-1"] < 2 {
		t.Fatalf("failed segment was not retried: %+v", counts)
	}
	if _, err := os.Stat(segmentDir); !os.IsNotExist(err) {
		t.Fatalf("segment directory was not cleaned up: %v", err)
	}
}
