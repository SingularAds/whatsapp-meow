package utils

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os/exec"
)

// TranscodeToWhatsAppOpus re-encodes any audio format to OGG/Opus using ffmpeg
// with the exact parameters WhatsApp mobile clients require:
//   - codec:      libopus
//   - bitrate:    32 kbps
//   - sample rate: 48 000 Hz
//   - channels:   1 (mono)
//
// If ffmpeg is not in PATH the function returns the original bytes unchanged
// (Web will still play; mobile may not — install ffmpeg to fix).
// If ffmpeg is found but fails the error is returned to the caller.
func TranscodeToWhatsAppOpus(data []byte) ([]byte, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		slog.Warn("ffmpeg not found in PATH — audio not re-encoded; mobile playback may fail")
		return data, nil
	}

	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-c:a", "libopus",
		"-b:a", "32k",
		"-ar", "48000",
		"-ac", "1",
		"-f", "ogg",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(data)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if runErr := cmd.Run(); runErr != nil {
		return nil, fmt.Errorf("ffmpeg transcode failed: %w — %s", runErr, stderr.String())
	}

	return stdout.Bytes(), nil
}

// GenerateWaveform produces a 64-sample amplitude waveform []byte where each
// byte is in [0, 100], suitable for waE2E.AudioMessage.Waveform.
//
// For OGG Opus streams it estimates per-frame energy by scanning OGG page
// body sizes — with VBR encoding, larger pages carry more audio content and
// therefore represent higher amplitude sections.  Falls back to a flat
// mid-level waveform for non-OGG or very short data.
func GenerateWaveform(data []byte) []byte {
	const samples = 64

	flat := func() []byte {
		out := make([]byte, samples)
		for i := range out {
			out[i] = 50
		}
		return out
	}

	const (
		magic     = "OggS"
		headerMin = 27
	)

	type pageInfo struct {
		posInAudio int // cumulative byte position among audio pages
		size       int
	}

	var pages []pageInfo
	audioBytePos := 0
	i := 0

	for i+headerMin <= len(data) {
		if data[i] != 'O' {
			i++
			continue
		}
		if i+4 > len(data) {
			break
		}
		if string(data[i:i+4]) != magic {
			i++
			continue
		}
		if i+headerMin > len(data) {
			break
		}

		numSegments := int(data[i+26])
		if i+headerMin+numSegments > len(data) {
			break
		}

		bodySize := 0
		for j := 0; j < numSegments; j++ {
			bodySize += int(data[i+headerMin+j])
		}

		pages = append(pages, pageInfo{posInAudio: audioBytePos, size: bodySize})
		audioBytePos += bodySize
		i += headerMin + numSegments + bodySize
	}

	// Skip first 2 OGG pages (ID header + comment header).
	if len(pages) <= 2 {
		return flat()
	}
	audioPages := pages[2:]
	if audioBytePos == 0 {
		return flat()
	}

	// Bucket audio pages into `samples` bins and sum body sizes per bin.
	buckets := make([]float64, samples)
	var maxBucket float64

	for _, p := range audioPages {
		b := int(float64(p.posInAudio) / float64(audioBytePos) * float64(samples))
		if b >= samples {
			b = samples - 1
		}
		buckets[b] += float64(p.size)
		if buckets[b] > maxBucket {
			maxBucket = buckets[b]
		}
	}

	if maxBucket == 0 {
		return flat()
	}

	out := make([]byte, samples)
	for i, v := range buckets {
		out[i] = byte(v / maxBucket * 100)
	}
	return out
}

// ExtractOGGDuration returns the duration in whole seconds of an OGG Opus audio stream.
//
// Method: scan every OGG page header (magic "OggS") for the 64-bit granule position
// field, keep the maximum value, then divide by 48 000 Hz (the fixed Opus sample rate).
//
// Falls back to 0 if the data is too short or no valid page is found.
func ExtractOGGDuration(data []byte) uint32 {
	const (
		magic       = "OggS"
		headerMin   = 27 // minimum OGG page header size (before segment table)
		opusSampleRate = 48000
	)

	var maxGranule uint64

	i := 0
	for i+headerMin <= len(data) {
		// Fast scan for 'O'
		if data[i] != 'O' {
			i++
			continue
		}
		if i+4 > len(data) {
			break
		}
		if string(data[i:i+4]) != magic {
			i++
			continue
		}

		// Check we can read the full header
		if i+headerMin > len(data) {
			break
		}

		// Granule position is at bytes 6–13 (little-endian int64).
		// Whatsmeow/Opus use the raw uint64 representation; negative values
		// (e.g. -1 = 0xFFFFFFFFFFFFFFFF) signal "no position" and are skipped.
		raw := binary.LittleEndian.Uint64(data[i+6 : i+14])
		if raw != 0xFFFFFFFFFFFFFFFF && raw > maxGranule {
			maxGranule = raw
		}

		// Number of lace segments is at byte 26.
		numSegments := int(data[i+26])
		if i+headerMin+numSegments > len(data) {
			break
		}

		// Sum segment table to compute total page body size.
		pageBodySize := 0
		for j := 0; j < numSegments; j++ {
			pageBodySize += int(data[i+headerMin+j])
		}

		i += headerMin + numSegments + pageBodySize
	}

	if maxGranule == 0 {
		return 0
	}
	return uint32(maxGranule / opusSampleRate)
}
