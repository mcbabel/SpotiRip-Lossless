package main

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"

	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

type oggPage struct {
	flags    byte
	granule  uint64
	serial   uint32
	sequence uint32
	segments []byte
	payload  []byte
}

type oggPacketLocation struct {
	startPage, startSegment int
	endPage, endSegment     int
}

func parseOggPages(data []byte) ([]oggPage, error) {
	var pages []oggPage
	for offset := 0; offset < len(data); {
		if offset+27 > len(data) || string(data[offset:offset+4]) != "OggS" {
			return nil, fmt.Errorf("invalid OGG page at %d", offset)
		}
		segmentCount := int(data[offset+26])
		headerSize := 27 + segmentCount
		if offset+headerSize > len(data) {
			return nil, fmt.Errorf("truncated OGG page")
		}
		segments := append([]byte(nil), data[offset+27:offset+headerSize]...)
		payloadSize := 0
		for _, size := range segments {
			payloadSize += int(size)
		}
		payloadStart := offset + headerSize
		if payloadStart+payloadSize > len(data) {
			return nil, fmt.Errorf("truncated OGG data")
		}
		pages = append(pages, oggPage{
			flags: data[offset+5], granule: binary.LittleEndian.Uint64(data[offset+6:]),
			serial: binary.LittleEndian.Uint32(data[offset+14:]), sequence: binary.LittleEndian.Uint32(data[offset+18:]),
			segments: segments, payload: append([]byte(nil), data[payloadStart:payloadStart+payloadSize]...),
		})
		offset = payloadStart + payloadSize
	}
	return pages, nil
}

func segmentOffset(page oggPage, segment int) int {
	offset := 0
	for _, size := range page.segments[:segment] {
		offset += int(size)
	}
	return offset
}

func findVorbisComment(pages []oggPage) (oggPacketLocation, []byte, error) {
	var packet []byte
	location := oggPacketLocation{startPage: -1}
	for pageIndex, page := range pages {
		for segmentIndex, size := range page.segments {
			if len(packet) == 0 {
				location.startPage = pageIndex
				location.startSegment = segmentIndex
			}
			offset := segmentOffset(page, segmentIndex)
			packet = append(packet, page.payload[offset:offset+int(size)]...)
			if size == 255 {
				continue
			}
			if len(packet) >= 8 && packet[0] == 3 && string(packet[1:7]) == "vorbis" {
				location.endPage = pageIndex
				location.endSegment = segmentIndex
				return location, packet, nil
			}
			packet = packet[:0]
		}
	}
	return location, nil, fmt.Errorf("Vorbis comment packet not found")
}

func buildVorbisComment(original []byte, track *metadatapb.Track, link string, picture []byte) ([]byte, error) {
	if len(original) < 8 {
		return nil, fmt.Errorf("invalid Vorbis comment packet")
	}
	vendorLength := binary.LittleEndian.Uint32(original[7:])
	vendorStart := 11
	commentCountOffset := vendorStart + int(vendorLength)
	if commentCountOffset+4 > len(original) {
		return nil, fmt.Errorf("truncated Vorbis vendor")
	}
	commentCount := binary.LittleEndian.Uint32(original[commentCountOffset:])
	cursor := commentCountOffset + 4
	comments := []string{}
	for i := uint32(0); i < commentCount; i++ {
		if cursor+4 > len(original) {
			return nil, fmt.Errorf("truncated Vorbis comment")
		}
		length := int(binary.LittleEndian.Uint32(original[cursor:]))
		cursor += 4
		if length < 0 || cursor+length > len(original) {
			return nil, fmt.Errorf("invalid Vorbis comment length")
		}
		comments = append(comments, string(original[cursor:cursor+length]))
		cursor += length
	}
	updates := trackTags(track, link)
	if len(picture) > 0 {
		updates["METADATA_BLOCK_PICTURE"] = base64.StdEncoding.EncodeToString(picture)
	}
	filtered := comments[:0]
	for _, comment := range comments {
		key, value, found := strings.Cut(comment, "=")
		if !found {
			filtered = append(filtered, comment)
			continue
		}
		key = strings.ToUpper(key)
		if _, managed := updates[key]; managed || key == "ALBUM_ARTIST" {
			continue
		}
		if (key == "COMMENT" || key == "DESCRIPTION") && canonicalTrack(value) != "" {
			continue
		}
		filtered = append(filtered, comment)
	}
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		filtered = append(filtered, key+"="+updates[key])
	}
	var output []byte
	output = append(output, original[:7]...)
	output = binary.LittleEndian.AppendUint32(output, vendorLength)
	output = append(output, original[vendorStart:commentCountOffset]...)
	output = binary.LittleEndian.AppendUint32(output, uint32(len(filtered)))
	for _, comment := range filtered {
		output = binary.LittleEndian.AppendUint32(output, uint32(len(comment)))
		output = append(output, comment...)
	}
	output = append(output, 1)
	return output, nil
}

func flacPicture(data []byte) []byte {
	mime := "application/octet-stream"
	switch {
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		mime = "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		mime = "image/png"
	}
	var output []byte
	output = binary.BigEndian.AppendUint32(output, 3)
	output = binary.BigEndian.AppendUint32(output, uint32(len(mime)))
	output = append(output, mime...)
	output = binary.BigEndian.AppendUint32(output, 0)
	output = append(output, make([]byte, 16)...)
	output = binary.BigEndian.AppendUint32(output, uint32(len(data)))
	output = append(output, data...)
	return output
}

func packetPages(serial uint32, packet []byte) []oggPage {
	segments := []byte{}
	remaining := len(packet)
	for remaining >= 255 {
		segments = append(segments, 255)
		remaining -= 255
	}
	segments = append(segments, byte(remaining))
	var pages []oggPage
	segmentOffset, payloadOffset := 0, 0
	for segmentOffset < len(segments) {
		count := len(segments) - segmentOffset
		if count > 255 {
			count = 255
		}
		pageSegments := segments[segmentOffset : segmentOffset+count]
		payloadSize := 0
		for _, size := range pageSegments {
			payloadSize += int(size)
		}
		flags := byte(0)
		if len(pages) > 0 {
			flags = 1
		}
		pages = append(pages, oggPage{flags: flags, serial: serial, segments: append([]byte(nil), pageSegments...), payload: packet[payloadOffset : payloadOffset+payloadSize]})
		segmentOffset += count
		payloadOffset += payloadSize
	}
	return pages
}

var oggCRCTable = func() [256]uint32 {
	var table [256]uint32
	for i := 0; i < 256; i++ {
		value := uint32(i) << 24
		for bit := 0; bit < 8; bit++ {
			if value&0x80000000 != 0 {
				value = value<<1 ^ 0x04c11db7
			} else {
				value <<= 1
			}
		}
		table[i] = value
	}
	return table
}()

func encodeOggPage(page oggPage) []byte {
	output := make([]byte, 27+len(page.segments)+len(page.payload))
	copy(output, "OggS")
	output[5] = page.flags
	output[26] = byte(len(page.segments))
	binary.LittleEndian.PutUint64(output[6:], page.granule)
	binary.LittleEndian.PutUint32(output[14:], page.serial)
	binary.LittleEndian.PutUint32(output[18:], page.sequence)
	copy(output[27:], page.segments)
	copy(output[27+len(page.segments):], page.payload)
	var crc uint32
	for _, value := range output {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^value]
	}
	binary.LittleEndian.PutUint32(output[22:], crc)
	return output
}

func embedOggMetadata(path, coverPath string, track *metadatapb.Track, link string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pages, err := parseOggPages(data)
	if err != nil {
		return err
	}
	location, packet, err := findVorbisComment(pages)
	if err != nil {
		return err
	}
	var picture []byte
	if coverPath != "" {
		cover, err := os.ReadFile(coverPath)
		if err != nil {
			return err
		}
		picture = flacPicture(cover)
	}
	updated, err := buildVorbisComment(packet, track, link, picture)
	if err != nil {
		return err
	}
	var rebuilt []oggPage
	rebuilt = append(rebuilt, pages[:location.startPage]...)
	startPage := pages[location.startPage]
	if location.startSegment > 0 {
		rebuilt = append(rebuilt, oggPage{flags: startPage.flags, granule: startPage.granule, serial: startPage.serial, segments: append([]byte(nil), startPage.segments[:location.startSegment]...), payload: append([]byte(nil), startPage.payload[:segmentOffset(startPage, location.startSegment)]...)})
	}
	rebuilt = append(rebuilt, packetPages(startPage.serial, updated)...)
	endPage := pages[location.endPage]
	if location.endSegment+1 < len(endPage.segments) {
		rebuilt = append(rebuilt, oggPage{granule: endPage.granule, serial: endPage.serial, segments: append([]byte(nil), endPage.segments[location.endSegment+1:]...), payload: append([]byte(nil), endPage.payload[segmentOffset(endPage, location.endSegment+1):]...)})
	}
	rebuilt = append(rebuilt, pages[location.endPage+1:]...)
	output := []byte{}
	for index, page := range rebuilt {
		page.sequence = uint32(index)
		output = append(output, encodeOggPage(page)...)
	}
	tagged := strings.TrimSuffix(path, ".ogg") + ".tagged.ogg"
	if err := os.WriteFile(tagged, output, 0o644); err != nil {
		return err
	}
	return os.Rename(tagged, path)
}
