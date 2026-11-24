package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	librespot "github.com/devgianlu/go-librespot"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/session"
)

type batchResult struct {
	link string
	err  error
}

func resolveLinks(ctx context.Context, options cliOptions) ([]string, error) {
	if info, err := os.Stat(options.link); err == nil && info.Mode().IsRegular() {
		data, err := os.ReadFile(options.link)
		if err != nil {
			return nil, err
		}
		var links []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			nested := options
			nested.link = line
			resolved, err := resolveLinks(ctx, nested)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", line, err)
			}
			links = append(links, resolved...)
		}
		return uniqueTrackLinks(links), nil
	}
	kind, id := splitSpotifyLink(options.link)
	if kind == "track" {
		return []string{options.link}, nil
	}
	if kind != "album" && kind != "artist" && kind != "playlist" {
		return nil, fmt.Errorf("a supported Spotify link or link list is required")
	}
	sess, _, err := openSession(ctx, options)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	sp := sess.Spclient()
	var links []string
	switch kind {
	case "album":
		albumID, err := librespot.SpotifyIdFromBase62(librespot.SpotifyIdTypeAlbum, id)
		if err != nil {
			return nil, err
		}
		var album metadatapb.Album
		if err := sp.ExtendedMetadataSimple(ctx, *albumID, extmetadatapb.ExtensionKind_ALBUM_V4, &album); err != nil {
			return nil, fmt.Errorf("album metadata: %w", err)
		}
		for _, disc := range album.Disc {
			for _, track := range disc.Track {
				links = append(links, trackLink(track))
			}
		}
	case "artist":
		artistID, err := librespot.SpotifyIdFromBase62(librespot.SpotifyIdTypeArtist, id)
		if err != nil {
			return nil, err
		}
		var artist metadatapb.Artist
		if err := sp.ExtendedMetadataSimple(ctx, *artistID, extmetadatapb.ExtensionKind_ARTIST_V4, &artist); err != nil {
			return nil, fmt.Errorf("artist metadata: %w", err)
		}
		for _, group := range artist.TopTrack {
			for _, track := range group.Track {
				links = append(links, trackLink(track))
			}
		}
		for _, group := range [][]*metadatapb.AlbumGroup{artist.AlbumGroup, artist.SingleGroup, artist.CompilationGroup, artist.AppearsOnGroup} {
			for _, albumGroup := range group {
				for _, album := range albumGroup.Album {
					for _, disc := range album.Disc {
						for _, track := range disc.Track {
							links = append(links, trackLink(track))
						}
					}
				}
			}
		}
	case "playlist":
		playlistID, err := librespot.SpotifyIdFromBase62(librespot.SpotifyIdTypePlaylist, id)
		if err != nil {
			return nil, err
		}
		playlist, err := sp.PlaylistContext(ctx, *playlistID)
		if err != nil {
			return nil, fmt.Errorf("Playlist: %w", err)
		}
		if len(playlist.Pages) == 0 {
			return nil, fmt.Errorf("playlist contains no track page")
		}
		page := playlist.Pages[0]
		for {
			for _, track := range page.Tracks {
				links = append(links, track.GetUri())
			}
			if page.NextPageUrl == "" {
				break
			}
			page, err = sp.PlaylistPage(ctx, page.NextPageUrl)
			if err != nil {
				return nil, fmt.Errorf("Playlist-Seite: %w", err)
			}
		}
	}
	return uniqueTrackLinks(links), nil
}

func splitSpotifyLink(link string) (string, string) {
	for _, prefix := range []string{"track", "album", "artist", "playlist"} {
		if strings.HasPrefix(link, "spotify:"+prefix+":") {
			return prefix, strings.TrimPrefix(link, "spotify:"+prefix+":")
		}
		marker := "/" + prefix + "/"
		if strings.Contains(link, marker) {
			tail := strings.SplitN(strings.SplitN(link, marker, 2)[1], "?", 2)[0]
			if prefix == "track" && strings.HasPrefix(tail, "spotify:track:") {
				return "track", tail
			}
			return prefix, tail
		}
	}
	if strings.HasPrefix(link, "spotify:track:") {
		return "track", strings.TrimPrefix(link, "spotify:track:")
	}
	return "", ""
}

func trackLink(track *metadatapb.Track) string {
	if uri := track.GetCanonicalUri(); uri != "" {
		return uri
	}
	if len(track.GetGid()) == 16 {
		return librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, track.GetGid()).Uri()
	}
	return ""
}

func uniqueTrackLinks(links []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, link := range links {
		canonical := canonicalTrack(link)
		if canonical == "" || seen[canonical] {
			continue
		}
		seen[canonical] = true
		result = append(result, canonical)
	}
	return result
}

func run(ctx context.Context, options cliOptions) error {
	if options.selfTest {
		sess, client, err := openSession(ctx, options)
		if err != nil {
			return err
		}
		defer sess.Close()
		return selfTest(ctx, sess.Spclient(), client)
	}
	links, err := resolveLinks(ctx, options)
	if err != nil {
		return err
	}
	if options.limit > 0 && len(links) > options.limit {
		links = links[:options.limit]
	}
	if len(links) == 0 {
		return fmt.Errorf("no tracks found to download")
	}
	if options.onDuplicate == "skip" {
		index, err := buildDuplicateIndex(options.outputDir)
		if err != nil {
			return err
		}
		links = filterDuplicateLinks(links, index)
		if len(links) == 0 {
			fmt.Println("All existing files will be skipped")
			return nil
		}
	}
	workers := options.concurrent
	if workers == 0 {
		workers = 2
	}
	if workers < 1 || workers > 3 {
		return fmt.Errorf("--concurrent must be between 1 and 3")
	}
	if options.selectFormat {
		workers = 1
	}
	jobs := make(chan string)
	results := make(chan batchResult, len(links))
	var workersWG sync.WaitGroup
	workersWG.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer workersWG.Done()
			var sess *session.Session
			var client *http.Client
			var sessionErr error
			defer func() {
				if sess != nil {
					sess.Close()
				}
			}()
			for link := range jobs {
				jobOptions := options
				jobOptions.link = link
				if sess == nil && sessionErr == nil {
					sess, client, sessionErr = openSession(ctx, options)
				}
				if sessionErr != nil {
					results <- batchResult{link: link, err: sessionErr}
					continue
				}
				results <- batchResult{link: link, err: download(ctx, jobOptions, sess, client)}
			}
		}()
	}
	go func() {
		for _, link := range links {
			jobs <- link
		}
		close(jobs)
	}()
	workersWG.Wait()
	close(results)
	var failures []string
	for result := range results {
		if result.err != nil {
			if errors.Is(result.err, errDuplicate) {
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %v", result.link, result.err))
		}
	}
	fmt.Printf("Downloads completed: %d succeeded, %d failed\n", len(links)-len(failures), len(failures))
	if len(failures) > 0 {
		return fmt.Errorf("failed downloads:\n%s", strings.Join(failures, "\n"))
	}
	return nil
}
