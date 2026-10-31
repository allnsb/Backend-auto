package emoji

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"odysseia.local/backend/internal/media"
)

func TestPrepareRejectsTypeSizeAndEdge(t *testing.T) {
	limits := DefaultLimits()
	if _, err := Prepare(mustJPEG(t), limits); !errors.Is(err, media.ErrType) {
		t.Fatalf("jpeg err = %v", err)
	}
	tiny := Limits{MaxBytes: 32, MaxEdge: 128}
	if _, err := Prepare(mustPNG(t, 1, 1), tiny); !errors.Is(err, media.ErrTooLarge) {
		t.Fatalf("oversize err = %v", err)
	}
	wide := Limits{MaxBytes: MaxBytesCeiling, MaxEdge: 128}
	if _, err := Prepare(mustPNG(t, 300, 1), wide); !errors.Is(err, media.ErrDimensions) {
		t.Fatalf("edge err = %v", err)
	}
	still, err := Prepare(mustPNG(t, 8, 8), limits)
	if err != nil || still.Animated || still.ContentType != "image/png" || still.Key == "" {
		t.Fatalf("png = %+v err=%v", still, err)
	}
	motion, err := Prepare(mustPNGFrames(t, 2), limits)
	if err != nil || !motion.Animated || motion.ContentType != "image/png" {
		t.Fatalf("apng = %+v err=%v", motion, err)
	}
	film, err := Prepare(mustGIF(t, 2), limits)
	if err != nil || !film.Animated || film.ContentType != "image/gif" {
		t.Fatalf("gif = %+v err=%v", film, err)
	}
	one, err := Prepare(mustGIF(t, 1), limits)
	if err != nil || one.Animated {
		t.Fatalf("single gif animated=%v err=%v", one.Animated, err)
	}
	web, err := Prepare(mustWebP(t, false), limits)
	if err != nil || web.Animated || web.ContentType != "image/webp" {
		t.Fatalf("webp = %+v err=%v", web, err)
	}
	moved, err := Prepare(mustWebP(t, true), limits)
	if err != nil || !moved.Animated {
		t.Fatalf("animated webp = %+v err=%v", moved, err)
	}
	for _, name := range []string{"a", "has-dash", "has space", "名字"} {
		if err := ValidName(name); !errors.Is(err, ErrInvalid) {
			t.Fatalf("name %q err=%v", name, err)
		}
	}
	if err := ValidName("wave_2"); err != nil {
		t.Fatal(err)
	}
}

func TestTokensResolveAndDegrade(t *testing.T) {
	id := "6d2f0c2e-1b7a-4e4e-9c1a-2f6b0d9a7c31"
	other := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	body := "hi <:wave:" + id + "> <a:spin:" + other + "> <:wave:" + id + ">"
	tokens, err := Parse(body)
	if err != nil || len(tokens) != 3 || !tokens[1].Animated || tokens[0].Name != "wave" {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
	live := map[string]Ref{
		id:    {ID: id, Name: "wave", Scope: "site", ContentType: "image/png", URL: "https://emoji.invalid/wave"},
		other: {ID: other, Name: "spin", Animated: true, Scope: "forum", ForumID: "11111111-1111-4111-8111-111111111111"},
	}
	if _, err := Resolve(tokens, live, map[string]struct{}{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("outsider forum emoji err=%v", err)
	}
	member := map[string]struct{}{live[other].ForumID: {}}
	refs, err := Resolve(tokens, live, member)
	if err != nil || len(refs) != 2 || refs[0].ID != id || refs[1].ID != other {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	static := tokens
	static[1].Animated = false
	if _, err := Resolve(static, live, member); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("flag mismatch err=%v", err)
	}
	delete(live, id)
	shown, left := Project(body, live)
	want := "hi :wave: <a:spin:" + other + "> :wave:"
	if shown != want || len(left) != 1 || left[0].ID != other {
		t.Fatalf("shown=%q left=%+v", shown, left)
	}
	plain := "no token here <:not-a-token>"
	if got, refs := Project(plain, live); got != plain || refs != nil {
		t.Fatalf("plain changed %q %+v", got, refs)
	}
}

func mustJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	img.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustPNGFrames(t *testing.T, frames int) []byte {
	t.Helper()
	raw := mustPNG(t, 2, 2)
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[:4], uint32(frames))
	chunk := pngChunk("acTL", payload)
	sig := 8
	ihdrLen := 8 + int(binary.BigEndian.Uint32(raw[sig:sig+4])) + 4
	out := append([]byte{}, raw[:sig+ihdrLen]...)
	out = append(out, chunk...)
	out = append(out, raw[sig+ihdrLen:]...)
	return out
}

func pngChunk(kind string, data []byte) []byte {
	buf := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
	copy(buf[4:8], kind)
	copy(buf[8:], data)
	sum := crc32.ChecksumIEEE(buf[4:])
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], sum)
	return append(buf, tail[:]...)
}

func mustGIF(t *testing.T, frames int) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	img := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	g := &gif.GIF{Config: image.Config{Width: 2, Height: 2, ColorModel: palette}}
	for range frames {
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 2)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustWebP(t *testing.T, animated bool) []byte {
	t.Helper()
	payload := make([]byte, 10)
	if animated {
		payload[0] = 0x02
	}
	payload[4] = 1
	payload[7] = 1
	chunk := func(id string, data []byte) []byte {
		out := make([]byte, 8+len(data))
		copy(out, id)
		binary.LittleEndian.PutUint32(out[4:8], uint32(len(data)))
		copy(out[8:], data)
		return out
	}
	body := append([]byte("WEBP"), chunk("VP8X", payload)...)
	out := make([]byte, 8+len(body))
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(body)))
	copy(out[8:], body)
	return out
}
