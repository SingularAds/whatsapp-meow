package utils_test

import (
	"encoding/binary"
	"testing"

	"whatsapp-bridge/utils"
)

// buildOGGPage builds a minimal OGG page binary with a specified granule position.
func buildOGGPage(granule uint64) []byte {
	// OGG page layout:
	// 0–3:   "OggS"
	// 4:     version (0)
	// 5:     header type (0)
	// 6–13:  granule position (little-endian int64)
	// 14–17: serial number
	// 18–21: sequence number
	// 22–25: checksum (we leave as 0 — our parser doesn't validate it)
	// 26:    number of segments
	// 27…:   segment table (1 segment of 0 bytes → empty page body)
	page := make([]byte, 27+1) // header + 1-byte segment table + 0-byte body
	copy(page[0:4], "OggS")
	binary.LittleEndian.PutUint64(page[6:14], granule)
	page[26] = 1 // 1 segment
	page[27] = 0 // segment size 0
	return page
}

func TestExtractOGGDuration_EmptyData(t *testing.T) {
	if got := utils.ExtractOGGDuration(nil); got != 0 {
		t.Errorf("expected 0 for nil input, got %d", got)
	}
	if got := utils.ExtractOGGDuration([]byte{}); got != 0 {
		t.Errorf("expected 0 for empty input, got %d", got)
	}
}

func TestExtractOGGDuration_NoValidPage(t *testing.T) {
	data := []byte("this is not an OGG file at all, no magic bytes")
	if got := utils.ExtractOGGDuration(data); got != 0 {
		t.Errorf("expected 0 for invalid data, got %d", got)
	}
}

func TestExtractOGGDuration_SentinelGranule(t *testing.T) {
	// Granule = 0xFFFFFFFFFFFFFFFF is the "no position" sentinel — must return 0.
	page := buildOGGPage(0xFFFFFFFFFFFFFFFF)
	if got := utils.ExtractOGGDuration(page); got != 0 {
		t.Errorf("expected 0 for sentinel granule, got %d", got)
	}
}

func TestExtractOGGDuration_10Seconds(t *testing.T) {
	// 10 seconds at 48 000 Hz = 480 000 granules.
	const granule = 480_000
	page := buildOGGPage(granule)
	got := utils.ExtractOGGDuration(page)
	if got != 10 {
		t.Errorf("expected 10, got %d", got)
	}
}

func TestExtractOGGDuration_MultiplePages(t *testing.T) {
	// Two pages: 5 s and 12 s. Parser should return the max (12 s).
	page5  := buildOGGPage(5 * 48_000)
	page12 := buildOGGPage(12 * 48_000)
	data := append(page5, page12...)
	got := utils.ExtractOGGDuration(data)
	if got != 12 {
		t.Errorf("expected 12, got %d", got)
	}
}

func TestExtractOGGDuration_ZeroGranule(t *testing.T) {
	page := buildOGGPage(0)
	if got := utils.ExtractOGGDuration(page); got != 0 {
		t.Errorf("expected 0 for zero granule, got %d", got)
	}
}
