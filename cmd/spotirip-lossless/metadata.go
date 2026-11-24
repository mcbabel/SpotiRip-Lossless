package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	librespot "github.com/devgianlu/go-librespot"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/spclient"
)

func largestCover(track *metadatapb.Track) *metadatapb.Image {
	album := track.GetAlbum()
	if album == nil {
		return nil
	}
	var selected *metadatapb.Image
	for _, group := range [][]*metadatapb.Image{album.GetCover(), album.GetCoverGroup().GetImage()} {
		for _, image := range group {
			if image == nil || len(image.GetFileId()) == 0 {
				continue
			}
			if selected == nil || image.GetWidth()*image.GetHeight() > selected.GetWidth()*selected.GetHeight() {
				selected = image
			}
		}
	}
	return selected
}

func ensureCoverMetadata(ctx context.Context, sp *spclient.Spclient, track *metadatapb.Track) error {
	if largestCover(track) != nil {
		return nil
	}
	album := track.GetAlbum()
	if album == nil || len(album.GetGid()) != 16 {
		return nil
	}
	albumID := librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeAlbum, album.GetGid())
	var details metadatapb.Album
	if err := sp.ExtendedMetadataSimple(ctx, albumID, extmetadatapb.ExtensionKind_ALBUM_V4, &details); err != nil {
		return fmt.Errorf("album cover: %w", err)
	}
	album.Cover = details.Cover
	album.CoverGroup = details.CoverGroup
	if album.Date == nil {
		album.Date = details.Date
	}
	if len(album.Copyright) == 0 {
		album.Copyright = details.Copyright
	}
	if len(album.Artist) == 0 {
		album.Artist = details.Artist
	}
	return nil
}

func downloadCover(ctx context.Context, client *http.Client, track *metadatapb.Track, outputDir string) (string, error) {
	image := largestCover(track)
	if image == nil {
		return "", nil
	}
	url := "https://i.scdn.co/image/" + hex.EncodeToString(image.GetFileId())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := doWithRetry(ctx, client, request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Cover HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32*1024*1024+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > 32*1024*1024 {
		return "", fmt.Errorf("invalid cover size")
	}
	file, err := os.CreateTemp(outputDir, ".cover-*.img")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func trackTags(track *metadatapb.Track, link string) map[string]string {
	artists := make([]string, 0, len(track.Artist))
	for _, artist := range track.Artist {
		if name := artist.GetName(); name != "" {
			artists = append(artists, name)
		}
	}
	albumArtist := strings.Join(artists, ", ")
	album := track.GetAlbum()
	if album != nil && len(album.Artist) > 0 && album.Artist[0].GetName() != "" {
		albumArtist = album.Artist[0].GetName()
	}
	values := map[string]string{
		"TITLE": track.GetName(), "ARTIST": strings.Join(artists, ", "),
		"ALBUMARTIST": albumArtist, "SPOTIFY_URL": link,
	}
	if number := track.GetNumber(); number > 0 {
		values["TRACKNUMBER"] = strconv.Itoa(int(number))
	}
	if number := track.GetDiscNumber(); number > 0 {
		values["DISCNUMBER"] = strconv.Itoa(int(number))
	}
	for _, externalID := range track.GetExternalId() {
		if strings.EqualFold(externalID.GetType(), "isrc") {
			values["ISRC"] = externalID.GetId()
		}
	}
	var composers []string
	for _, credit := range track.GetArtistWithRole() {
		if credit.GetRole() == metadatapb.ArtistWithRole_ARTIST_ROLE_COMPOSER && credit.GetArtistName() != "" {
			composers = append(composers, credit.GetArtistName())
		}
	}
	if len(composers) > 0 {
		values["COMPOSER"] = strings.Join(composers, ", ")
	}
	if album == nil {
		return values
	}
	values["ALBUM"] = album.GetName()
	if date := album.GetDate(); date.GetYear() > 0 {
		value := strconv.Itoa(int(date.GetYear()))
		if date.GetMonth() > 0 {
			value += fmt.Sprintf("-%02d", date.GetMonth())
			if date.GetDay() > 0 {
				value += fmt.Sprintf("-%02d", date.GetDay())
			}
		}
		values["DATE"] = value
	}
	var copyrights []string
	for _, copyright := range album.GetCopyright() {
		if copyright.GetText() != "" {
			copyrights = append(copyrights, copyright.GetText())
		}
	}
	if len(copyrights) > 0 {
		values["COPYRIGHT"] = strings.Join(copyrights, "\n")
	}
	return values
}

func metadataArguments(track *metadatapb.Track, link string) []string {
	values := trackTags(track, link)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	arguments := make([]string, 0, len(values)*2)
	for _, key := range keys {
		name := strings.ToLower(key)
		switch key {
		case "ALBUMARTIST":
			name = "album_artist"
		case "TRACKNUMBER":
			name = "track"
		case "DISCNUMBER":
			name = "disc"
		case "SPOTIFY_URL", "ISRC":
			name = key
		}
		if values[key] != "" {
			arguments = append(arguments, "-metadata", name+"="+values[key])
		}
	}
	return arguments
}

func embedMetadata(ctx context.Context, client *http.Client, track *metadatapb.Track, link, path, outputDir string) error {
	cover, err := downloadCover(ctx, client, track, outputDir)
	if err != nil {
		return err
	}
	if cover != "" {
		defer os.Remove(cover)
	}
	extension := filepath.Ext(path)
	if extension == ".ogg" {
		return embedOggMetadata(path, cover, track, link)
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg is required")
	}
	tagged := strings.TrimSuffix(path, extension) + ".tagged" + extension
	arguments := []string{"-y", "-loglevel", "error", "-i", path}
	if cover != "" {
		arguments = append(arguments, "-i", cover)
	}
	arguments = append(arguments, "-map", "0", "-map_metadata", "-1")
	if cover != "" {
		arguments = append(arguments, "-map", "1")
	}
	arguments = append(arguments, "-c", "copy")
	arguments = append(arguments, metadataArguments(track, link)...)
	if cover != "" {
		coverIndex := 0
		if extension == ".mp4" {
			ffprobe, err := exec.LookPath("ffprobe")
			if err != nil {
				return fmt.Errorf("ffprobe is required for the video cover: %w", err)
			}
			output, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v", "-show_entries", "stream=index", "-of", "csv=p=0", path).Output()
			if err != nil {
				return fmt.Errorf("could not count video streams: %w", err)
			}
			coverIndex = len(strings.Fields(string(output)))
		}
		index := strconv.Itoa(coverIndex)
		arguments = append(arguments, "-disposition:v:"+index, "attached_pic", "-metadata:s:v:"+index, "title=Album cover")
	}
	if extension == ".mp4" || extension == ".m4a" {
		arguments = append(arguments, "-movflags", "+faststart+use_metadata_tags")
	}
	if extension == ".mp3" {
		arguments = append(arguments, "-id3v2_version", "3")
	}
	arguments = append(arguments, tagged)
	command := exec.CommandContext(ctx, ffmpeg, arguments...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		os.Remove(tagged)
		return fmt.Errorf("FFmpeg metadata: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return os.Rename(tagged, path)
}
