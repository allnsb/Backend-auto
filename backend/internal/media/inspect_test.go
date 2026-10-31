package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInspectImagesStripsMetadataAndRejectsMarkup(t *testing.T) {
	limits := DefaultLimits()
	jpg := mustJPEG(t)
	tagged := injectJPEGEXIF(jpg, "GPSLatitude-secret")
	got, err := Inspect(tagged, 0, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentType != "image/jpeg" || got.Kind != "image" || bytes.Contains(got.Bytes, []byte("GPSLatitude-secret")) || bytes.Contains(got.Bytes, []byte("Exif")) {
		t.Fatalf("jpeg metadata survived type=%s kind=%s", got.ContentType, got.Kind)
	}
	if _, err := jpeg.DecodeConfig(bytes.NewReader(got.Bytes)); err != nil {
		t.Fatal(err)
	}
	again, err := Inspect(got.Bytes, 0, limits)
	if err != nil || !bytes.Equal(again.Bytes, got.Bytes) {
		t.Fatalf("jpeg strip is not stable: %v", err)
	}

	pngBytes := injectPNGText(mustPNG(t), "GPSLatitude-secret")
	got, err = Inspect(pngBytes, 0, limits)
	if err != nil || bytes.Contains(got.Bytes, []byte("GPSLatitude-secret")) || got.ContentType != "image/png" {
		t.Fatalf("png text survived err=%v type=%s", err, got.ContentType)
	}

	gifBytes := mustGIF(t)
	got, err = Inspect(gifBytes, 0, limits)
	if err != nil || !bytes.Equal(got.Bytes, gifBytes) || got.ContentType != "image/gif" {
		t.Fatalf("gif should pass through unchanged: %v", err)
	}

	webpBytes := webP(webpEXIF, 8, 8, riffChunk("EXIF", []byte("Exif\x00\x00GPSLatitude-secret")))
	got, err = Inspect(webpBytes, 0, limits)
	if err != nil || bytes.Contains(got.Bytes, []byte("GPSLatitude-secret")) || got.ContentType != "image/webp" || got.Width != 8 || got.Height != 8 {
		t.Fatalf("webp err=%v type=%s %dx%d", err, got.ContentType, got.Width, got.Height)
	}
	if len(got.Bytes) < 21 || got.Bytes[20]&(webpEXIF|webpXMP) != 0 {
		t.Fatalf("webp metadata flag remains %#x", got.Bytes[20])
	}

	for _, raw := range [][]byte{
		[]byte("<svg xmlns='http://www.w3.org/2000/svg'/>"),
		[]byte("<!DOCTYPE html><html></html>"),
		[]byte{0xEF, 0xBB, 0xBF, '<', 's', 'v', 'g'},
		[]byte("not-a-media-file"),
	} {
		if _, err := Inspect(raw, 0, limits); err != ErrType {
			t.Fatalf("markup/unknown err=%v", err)
		}
	}
	if _, err := Inspect(jpg, 1500, limits); err != ErrDuration {
		t.Fatalf("image duration err=%v", err)
	}

	wide := mustWideJPEG(t)
	rotated := injectOrientation(wide, 6, "GPSLatitude-secret")
	got, err = Inspect(rotated, 0, limits)
	if err != nil || got.Width != 2 || got.Height != 4 || jpegOrientation(got.Bytes) != 6 || bytes.Contains(got.Bytes, []byte("GPSLatitude-secret")) {
		t.Fatalf("oriented jpeg %dx%d tag=%d err=%v", got.Width, got.Height, jpegOrientation(got.Bytes), err)
	}

	commented := withGIFComment(gifBytes, "GPSLatitude-secret")
	got, err = Inspect(commented, 0, limits)
	if err != nil || bytes.Contains(got.Bytes, []byte("GPSLatitude-secret")) || got.ContentType != "image/gif" {
		t.Fatalf("gif comment survived err=%v", err)
	}
}

func TestInspectRejectsOversizedImagesAndLongEdges(t *testing.T) {
	limits := DefaultLimits()
	jpg := mustJPEG(t)
	if _, err := Inspect(jpg, 0, Limits{MaxBytes: int64(len(jpg) - 1), MaxEdge: limits.MaxEdge, MaxPixels: limits.MaxPixels, MaxVoiceMillis: limits.MaxVoiceMillis}); err != ErrTooLarge {
		t.Fatalf("size err=%v", err)
	}
	wide := pngWithSize(t, 9000, 1)
	if _, err := Inspect(wide, 0, limits); err != ErrDimensions {
		t.Fatalf("edge err=%v", err)
	}
	pixels := pngWithSize(t, 5000, 5000)
	tight := limits
	tight.MaxPixels = 1000
	if _, err := Inspect(pixels, 0, tight); err != ErrDimensions {
		t.Fatalf("pixel err=%v", err)
	}
}

func TestInspectVoiceDuration(t *testing.T) {
	limits := DefaultLimits()
	webm := buildWebM(1500, 2, "A_OPUS")
	got, err := Inspect(webm, 1500, limits)
	if err != nil || got.ContentType != "audio/webm;codecs=opus" || got.DurationMS != 1500 {
		t.Fatalf("webm duration=%d err=%v type=%s", got.DurationMS, err, got.ContentType)
	}
	if _, err := Inspect(webm, 20_000, limits); err != ErrDuration {
		t.Fatalf("mismatched client duration err=%v", err)
	}
	if _, err := Inspect(buildWebM(400_000, 2, "A_OPUS"), 400_000, limits); err != ErrDuration {
		t.Fatalf("long webm err=%v", err)
	}
	if _, err := Inspect(buildWebM(1500, 1, "V_VP8"), 1500, limits); err != ErrType {
		t.Fatalf("video webm err=%v", err)
	}

	mp4 := buildMP4(1500, "soun")
	got, err = Inspect(mp4, 1600, limits)
	if err != nil || got.ContentType != "audio/mp4" || got.DurationMS != 1500 {
		t.Fatalf("mp4 duration=%d err=%v", got.DurationMS, err)
	}
	if _, err := Inspect(buildMP4(1500, "soun", "vide"), 1500, limits); err != ErrType {
		t.Fatalf("video mp4 err=%v", err)
	}
	if _, err := Inspect(buildMP4(300_001, "soun"), 300_001, limits); err != ErrDuration {
		t.Fatalf("long mp4 err=%v", err)
	}

	ogg := buildOpus(48000, 0)
	got, err = Inspect(ogg, 1000, limits)
	if err != nil || got.ContentType != "audio/ogg" || got.DurationMS != 1000 {
		t.Fatalf("ogg duration=%d err=%v", got.DurationMS, err)
	}

	short := buildWebM(0, 2, "A_OPUS")
	if len(short) < 32 {
		t.Fatalf("fixture too small to exercise the floor: %d", len(short))
	}
	got, err = Inspect(short, 1000, limits)
	if err != nil || got.DurationMS != 1000 {
		t.Fatalf("missing container duration stored=%d err=%v", got.DurationMS, err)
	}
	if _, err := Inspect(short, 300_000, limits); err != ErrDuration {
		t.Fatalf("tiny file claiming five minutes err=%v", err)
	}

	tagged := append(buildWebM(1500, 2, "A_OPUS"), webmLocationTag()...)
	got, err = Inspect(tagged, 1500, limits)
	if err != nil || got.DurationMS != 1500 || bytes.Contains(got.Bytes, []byte("+37.0+122.0")) {
		t.Fatalf("webm tags duration=%d err=%v", got.DurationMS, err)
	}

	located := append(buildMP4(1500, "soun"), mp4Box("udta", mp4Box("\xa9xyz", []byte("+37.7749-122.4194/")))...)
	got, err = Inspect(located, 1500, limits)
	if err != nil || got.DurationMS != 1500 || bytes.Contains(got.Bytes, []byte("+37.7749-122.4194/")) {
		t.Fatalf("mp4 location duration=%d err=%v", got.DurationMS, err)
	}

	fragment := buildFragmentedMP4(1500, 1000)
	got, err = Inspect(fragment, 1500, limits)
	if err != nil || got.ContentType != "audio/mp4" || got.DurationMS != 1500 {
		t.Fatalf("fragmented mp4 duration=%d err=%v", got.DurationMS, err)
	}
	if _, err := Inspect(buildFragmentedMP4(400_000, 1000), 400_000, limits); err != ErrDuration {
		t.Fatalf("long fragmented mp4 err=%v", err)
	}
	// 5s at 48kHz is 240000 ticks. Dividing by the movie scale (1000) yields ~240s.
	mediaTicks := parseMust(t, buildFragmentedMP4(5*48000, 48000))
	if mediaTicks != 5000 {
		t.Fatalf("mdhd timescale duration=%d", mediaTicks)
	}
	unspecified := buildMP4(0, "soun")
	got, err = Inspect(unspecified, 1500, limits)
	if err != nil || got.DurationMS != 1500 {
		t.Fatalf("mvhd zero duration=%d err=%v", got.DurationMS, err)
	}
}

func mustWideJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 4, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func injectOrientation(data []byte, orientation byte, secret string) []byte {
	payload := append(minimalOrientationSegment(orientation)[4:], secret...)
	segment := make([]byte, 4+len(payload))
	segment[0], segment[1] = 0xFF, 0xE1
	binary.BigEndian.PutUint16(segment[2:4], uint16(2+len(payload)))
	copy(segment[4:], payload)
	out := append([]byte{}, data[:2]...)
	out = append(out, segment...)
	return append(out, data[2:]...)
}

func withGIFComment(data []byte, text string) []byte {
	block := []byte{0x21, 0xFE, byte(len(text))}
	block = append(block, text...)
	block = append(block, 0)
	end := bytes.LastIndexByte(data, 0x3B)
	out := append([]byte{}, data[:end]...)
	out = append(out, block...)
	return append(out, 0x3B)
}

func webmLocationTag() []byte {
	name := ebmlElement([]byte{0x45, 0xA3}, []byte("GPS_POSITION"))
	value := ebmlElement([]byte{0x44, 0x87}, []byte("+37.0+122.0"))
	simple := ebmlElement([]byte{0x67, 0xC8}, append(name, value...))
	return ebmlElement([]byte{0x12, 0x54, 0xC3, 0x67}, simple)
}

func buildFragmentedMP4(sampleTicks, mediaScale int) []byte {
	ftyp := mp4Box("ftyp", []byte("M4A \x00\x00\x00\x00M4A "))
	mvhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	tkhd := make([]byte, 24)
	binary.BigEndian.PutUint32(tkhd[12:16], 1)
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], uint32(mediaScale))
	hdlr := make([]byte, 12)
	copy(hdlr[8:12], "soun")
	mdia := append(mp4Box("mdhd", mdhd), mp4Box("hdlr", hdlr)...)
	trak := append(mp4Box("tkhd", tkhd), mp4Box("mdia", mdia)...)
	moov := append(mp4Box("mvhd", mvhd), mp4Box("trak", trak)...)
	tfhd := make([]byte, 8)
	binary.BigEndian.PutUint32(tfhd[4:8], 1)
	trun := make([]byte, 12)
	binary.BigEndian.PutUint32(trun[0:4], 0x000100)
	binary.BigEndian.PutUint32(trun[4:8], 1)
	binary.BigEndian.PutUint32(trun[8:12], uint32(sampleTicks))
	moof := mp4Box("moof", mp4Box("traf", append(mp4Box("tfhd", tfhd), mp4Box("trun", trun)...)))
	return append(append(ftyp, mp4Box("moov", moov)...), moof...)
}

func parseMust(t *testing.T, data []byte) int {
	t.Helper()
	ms, err := parseMP4(data)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func mustJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustGIF(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	palette := color.Palette{color.Black, color.White}
	if err := gif.Encode(&buf, image.NewPaletted(image.Rect(0, 0, 2, 2), palette), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func injectJPEGEXIF(data []byte, marker string) []byte {
	payload := append([]byte("Exif\x00\x00"), marker...)
	seg := make([]byte, 4+len(payload))
	seg[0], seg[1] = 0xFF, 0xE1
	binary.BigEndian.PutUint16(seg[2:4], uint16(2+len(payload)))
	copy(seg[4:], payload)
	out := append([]byte{}, data[:2]...)
	out = append(out, seg...)
	return append(out, data[2:]...)
}

func injectPNGText(data []byte, secret string) []byte {
	iend := bytes.LastIndex(data, []byte("IEND"))
	if iend < 4 {
		panic("png missing IEND")
	}
	chunkStart := iend - 4
	text := append([]byte("Comment\x00"), secret...)
	chunk := pngChunk("tEXt", text)
	out := append([]byte{}, data[:chunkStart]...)
	out = append(out, chunk...)
	return append(out, data[chunkStart:]...)
}

func pngWithSize(t *testing.T, width, height int) []byte {
	t.Helper()
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(height))
	ihdr[8] = 8
	ihdr[9] = 2
	out := append([]byte{}, pngSignature...)
	out = append(out, pngChunk("IHDR", ihdr)...)
	out = append(out, pngChunk("IEND", nil)...)
	if _, err := png.DecodeConfig(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
	return out
}

func pngChunk(kind string, data []byte) []byte {
	buf := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(data)))
	copy(buf[4:8], kind)
	copy(buf[8:], data)
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], crc32.ChecksumIEEE(buf[4:]))
	return append(buf, tail[:]...)
}

func webP(flags byte, width, height int, extra []byte) []byte {
	payload := make([]byte, 10)
	payload[0] = flags
	w, h := width-1, height-1
	payload[4] = byte(w)
	payload[5] = byte(w >> 8)
	payload[6] = byte(w >> 16)
	payload[7] = byte(h)
	payload[8] = byte(h >> 8)
	payload[9] = byte(h >> 16)
	body := append([]byte("WEBP"), riffChunk("VP8X", payload)...)
	body = append(body, extra...)
	out := make([]byte, 8+len(body))
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(body)))
	copy(out[8:], body)
	return out
}

func riffChunk(id string, data []byte) []byte {
	out := make([]byte, 8+len(data))
	copy(out, id)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(data)))
	copy(out[8:], data)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func buildMP4(durationMS int, handlers ...string) []byte {
	ftyp := mp4Box("ftyp", []byte("M4A \x00\x00\x00\x00M4A "))
	mvhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(durationMS))
	payload := mp4Box("mvhd", mvhd)
	for _, handler := range handlers {
		hdlr := make([]byte, 12)
		copy(hdlr[8:12], handler)
		payload = append(payload, mp4Box("trak", mp4Box("mdia", mp4Box("hdlr", hdlr)))...)
	}
	return append(ftyp, mp4Box("moov", payload)...)
}

func mp4Box(kind string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], kind)
	copy(out[8:], payload)
	return out
}

func buildOpus(granule int64, preSkip uint16) []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1
	head[9] = 1
	binary.LittleEndian.PutUint16(head[10:12], preSkip)
	binary.LittleEndian.PutUint32(head[12:16], 48000)
	first := oggPage(1, 0, 0, 0x02, head)
	second := oggPage(1, 1, granule, 0x04, []byte("Opus packet"))
	return append(first, second...)
}

func oggPage(serial, seq uint32, granule int64, header byte, payload []byte) []byte {
	page := make([]byte, 28+len(payload))
	copy(page, "OggS")
	page[5] = header
	binary.LittleEndian.PutUint64(page[6:14], uint64(granule))
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], seq)
	page[26] = 1
	page[27] = byte(len(payload))
	copy(page[28:], payload)
	return page
}

func buildWebM(durationMS int, trackType byte, codec string) []byte {
	header := ebmlElement([]byte{0x1A, 0x45, 0xDF, 0xA3}, ebmlElement([]byte{0x42, 0x82}, []byte("webm")))
	var infoPayload []byte
	infoPayload = append(infoPayload, ebmlElement([]byte{0x2A, 0xD7, 0xB1}, []byte{0x0F, 0x42, 0x40})...)
	if durationMS > 0 {
		var bits [8]byte
		// TimestampScale is 1e6 ns, so Duration is already in milliseconds.
		binary.BigEndian.PutUint64(bits[:], math.Float64bits(float64(durationMS)))
		infoPayload = append(infoPayload, ebmlElement([]byte{0x44, 0x89}, bits[:])...)
	}
	track := ebmlElement([]byte{0xAE}, append(
		ebmlElement([]byte{0x83}, []byte{trackType}),
		ebmlElement([]byte{0x86}, []byte(codec))...,
	))
	segment := ebmlElement([]byte{0x18, 0x53, 0x80, 0x67}, append(ebmlElement([]byte{0x15, 0x49, 0xA9, 0x66}, infoPayload), ebmlElement([]byte{0x16, 0x54, 0xAE, 0x6B}, track)...))
	return append(header, segment...)
}

func ebmlElement(id, payload []byte) []byte {
	return append(append(append([]byte{}, id...), ebmlSize(len(payload))...), payload...)
}

func ebmlSize(n int) []byte {
	if n < 0x7F {
		return []byte{0x80 | byte(n)}
	}
	return []byte{0x40 | byte(n>>8), byte(n)}
}

func TestFFmpegMP4StaysPlayableAfterLocationStrip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Fatal(err)
	}
	fast := renderMP4(t, "0.3", "+faststart", "-metadata", "location=+37.0000-122.0000/")
	cleaned, err := stripMP4Location(fast)
	if err != nil || len(cleaned) != len(fast) || bytes.Contains(cleaned, []byte("loci")) || bytes.Contains(cleaned, []byte("earth")) {
		t.Fatalf("faststart strip len=%d/%d err=%v", len(cleaned), len(fast), err)
	}
	if payload := boxPayload(t, fast, "stco"); payload == nil || !bytes.Equal(payload, boxPayload(t, cleaned, "stco")) {
		t.Fatal("stco offsets changed")
	}
	assertMP4Decodable(t, cleaned)
	if ms := parseMust(t, cleaned); ms < 250 || ms > 400 {
		t.Fatalf("faststart duration=%d", ms)
	}

	frag := renderMP4(t, "5", "frag_keyframe+empty_moov+default_base_moof")
	cleaned, err = stripMP4Location(frag)
	if err != nil || len(cleaned) != len(frag) {
		t.Fatalf("fragment strip err=%v", err)
	}
	if payload := boxPayload(t, frag, "tfhd"); payload == nil || !bytes.Equal(payload, boxPayload(t, cleaned, "tfhd")) {
		t.Fatal("tfhd offsets changed")
	}
	ms := parseMust(t, cleaned)
	if ms < 4800 || ms > 5300 {
		t.Fatalf("fragment duration=%d, want about 5000", ms)
	}
	assertMP4Decodable(t, cleaned)
}

func renderMP4(t *testing.T, duration, movflags string, extra ...string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.m4a")
	args := []string{"-y", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono", "-t", duration, "-c:a", "aac", "-movflags", movflags}
	args = append(args, extra...)
	args = append(args, path)
	out, err := exec.Command("ffmpeg", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertMP4Decodable(t *testing.T, data []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.m4a")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("decode: %v %s", err, out)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(probe)), 64)
	if err != nil || seconds <= 0 {
		t.Fatalf("ffprobe duration %q", probe)
	}
}

func boxPayload(t *testing.T, data []byte, kind string) []byte {
	t.Helper()
	i := 0
	for i+8 <= len(data) {
		box, err := readMP4Box(data, i)
		if err != nil {
			t.Fatal(err)
		}
		if box.kind == kind {
			return append([]byte(nil), data[box.payload:box.end]...)
		}
		if mp4Container(box.kind) {
			if found := boxPayload(t, data[box.payload:box.end], kind); found != nil {
				return found
			}
		}
		i = box.end
	}
	return nil
}
