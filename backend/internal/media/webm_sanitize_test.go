package media

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWebMSanitizerPreservesPlayback(t *testing.T) {
	data, err := os.ReadFile("testdata/location-opus.webm")
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), data...)
	got, err := Inspect(data, 1000, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Bytes) != len(data) || bytes.Contains(got.Bytes, []byte("+25.0000+121.0000/")) {
		t.Fatal("sanitizer must remove coordinates without shifting indexed elements")
	}
	if !bytes.Equal(data, before) {
		t.Fatal("inspection modified caller-owned bytes")
	}
	file := filepath.Join(t.TempDir(), "sanitized.webm")
	if err := os.WriteFile(file, got.Bytes, 0600); err != nil {
		t.Fatal(err)
	}
	// A real encoder fixture plus a decoder checks indexed container validity,
	// rather than repeating the sanitizer's element traversal in the assertion.
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", file, "-f", "null", "-")
	if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("sanitized WebM decoding: %v\n%s", err, output)
	}
}

func TestWebMSanitizerChecksUnknownSizeTail(t *testing.T) {
	data, err := os.ReadFile("testdata/location-opus.webm")
	if err != nil {
		t.Fatal(err)
	}
	segment := bytes.Index(data, []byte{0x18, 0x53, 0x80, 0x67})
	if segment < 0 {
		t.Fatal("fixture has no Segment")
	}
	_, _, width, err := readEBMLSize(data, segment+4)
	if err != nil || width != 8 {
		t.Fatalf("fixture Segment size width=%d error=%v", width, err)
	}
	copy(data[segment+4:], []byte{1, 255, 255, 255, 255, 255, 255, 255})
	if _, err := Inspect(data, 1000, DefaultLimits()); err != nil {
		t.Fatalf("valid unknown-size Segment: %v", err)
	}
	data = append(data, 0)
	if _, err := Inspect(data, 1000, DefaultLimits()); err != ErrCorrupt {
		t.Fatalf("malformed tail must reject instead of preserving coordinates: %v", err)
	}
}

func TestWebMSanitizerFindsTagsAfterUnknownCluster(t *testing.T) {
	data := buildWebM(1500, 2, "A_OPUS")
	segment := bytes.Index(data, []byte{0x18, 0x53, 0x80, 0x67})
	_, _, width, err := readEBMLSize(data, segment+4)
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte(nil), data[segment+4+width:]...)
	data = append(data[:segment+4], 0xFF) // Unknown-size Segment.
	data = append(data, payload...)
	data = append(data, 0x1F, 0x43, 0xB6, 0x75, 0xFF) // Unknown-size Cluster.
	data = append(data, ebmlElement([]byte{0xE7}, []byte{0})...)
	data = append(data, webmLocationTag()...)
	got, err := Inspect(data, 1500, DefaultLimits())
	if err != nil || bytes.Contains(got.Bytes, []byte("+37.0+122.0")) {
		t.Fatalf("Tags after streaming Cluster: %v", err)
	}
}
