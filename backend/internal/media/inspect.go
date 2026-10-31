package media

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/webp"
)

// Prepared is the bytes that will be stored, after metadata stripping for
// JPEG, PNG, and WebP. GIF is kept as uploaded so animation survives; GIF has
// no EXIF GPS block. Sum covers the stored bytes.
type Prepared struct {
	Kind        string
	ContentType string
	Bytes       []byte
	Width       int
	Height      int
	DurationMS  int
	Sum         [32]byte
}

// FileName is a server-chosen name. The upload filename is never used.
func (p Prepared) FileName() string {
	if name := fileName(p.ContentType); name != "" {
		return name
	}
	return "attachment.bin"
}

// Disposition is an inline download name with no caller-controlled characters.
func (p Prepared) Disposition() string {
	value, _ := ContentDisposition(p.ContentType)
	return value
}

// ContentDisposition maps an allowlisted type to a fixed download name.
func ContentDisposition(contentType string) (string, error) {
	name := fileName(contentType)
	if name == "" {
		return "", ErrType
	}
	return `inline; filename="` + name + `"`, nil
}

func fileName(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return "image.jpg"
	case "image/png":
		return "image.png"
	case "image/webp":
		return "image.webp"
	case "image/gif":
		return "image.gif"
	case "audio/webm;codecs=opus":
		return "voice.webm"
	case "audio/mp4":
		return "voice.mp4"
	case "audio/ogg":
		return "voice.ogg"
	default:
		return ""
	}
}

// Check confirms a value produced by Inspect still matches the process limits.
// It does not walk the container again.
func (p Prepared) Check(limits Limits) error {
	if err := limits.Valid(); err != nil {
		return err
	}
	if int64(len(p.Bytes)) > limits.MaxBytes {
		return ErrTooLarge
	}
	if len(p.Bytes) == 0 {
		return ErrCorrupt
	}
	if _, err := ContentDisposition(p.ContentType); err != nil {
		return err
	}
	if p.Sum != sha256.Sum256(p.Bytes) {
		return ErrCorrupt
	}
	if p.Kind == "image" {
		if p.DurationMS != 0 {
			return ErrDuration
		}
		return checkDimensions(p.Width, p.Height, limits)
	}
	if p.Kind != "voice" {
		return ErrType
	}
	if p.Width != 0 || p.Height != 0 || p.DurationMS < 1 || p.DurationMS > limits.MaxVoiceMillis {
		return ErrDuration
	}
	return nil
}

// Inspect sniffs the allowlisted container, rejects markup and unknown types,
// checks declared image dimensions, strips image metadata, and checks voice
// duration. declaredDurationMS is required for voice and must be zero for an image.
func Inspect(data []byte, declaredDurationMS int, limits Limits) (Prepared, error) {
	if err := limits.Valid(); err != nil {
		return Prepared{}, err
	}
	if int64(len(data)) > limits.MaxBytes {
		return Prepared{}, ErrTooLarge
	}
	if len(data) == 0 {
		return Prepared{}, ErrCorrupt
	}
	if leadingMarkup(data) {
		return Prepared{}, ErrType
	}
	switch {
	case isJPEG(data):
		return inspectImage(data, "image/jpeg", declaredDurationMS, limits, jpeg.DecodeConfig, stripJPEG)
	case isPNG(data):
		return inspectImage(data, "image/png", declaredDurationMS, limits, png.DecodeConfig, stripPNG)
	case isGIF(data):
		return inspectImage(data, "image/gif", declaredDurationMS, limits, gif.DecodeConfig, keepGIF)
	case isWebP(data):
		return inspectImage(data, "image/webp", declaredDurationMS, limits, webp.DecodeConfig, stripWebP)
	case isWebM(data):
		return inspectVoice(data, "audio/webm;codecs=opus", declaredDurationMS, limits, durationWebM)
	case isMP4(data):
		return inspectVoice(data, "audio/mp4", declaredDurationMS, limits, parseMP4)
	case isOgg(data):
		return inspectVoice(data, "audio/ogg", declaredDurationMS, limits, parseOgg)
	default:
		return Prepared{}, ErrType
	}
}

func inspectImage(data []byte, contentType string, declared int, limits Limits, decode func(io.Reader) (image.Config, error), strip func([]byte) ([]byte, error)) (Prepared, error) {
	if declared != 0 {
		return Prepared{}, ErrDuration
	}
	cfg, err := decode(bytes.NewReader(data))
	if err != nil {
		return Prepared{}, ErrCorrupt
	}
	if err := checkDimensions(cfg.Width, cfg.Height, limits); err != nil {
		return Prepared{}, err
	}
	stored, err := strip(data)
	if err != nil {
		return Prepared{}, err
	}
	if int64(len(stored)) > limits.MaxBytes || len(stored) == 0 {
		return Prepared{}, ErrCorrupt
	}
	return seal("image", contentType, stored, cfg.Width, cfg.Height, 0), nil
}

func inspectVoice(data []byte, contentType string, declared int, limits Limits, parse func([]byte) (int, error)) (Prepared, error) {
	parsed, err := parse(data)
	if err != nil {
		return Prepared{}, err
	}
	stored, err := acceptVoice(parsed, declared, len(data), limits)
	if err != nil {
		return Prepared{}, err
	}
	return seal("voice", contentType, data, 0, 0, stored), nil
}

func seal(kind, contentType string, stored []byte, width, height, duration int) Prepared {
	return Prepared{
		Kind: kind, ContentType: contentType, Bytes: stored,
		Width: width, Height: height, DurationMS: duration, Sum: sha256.Sum256(stored),
	}
}

func checkDimensions(width, height int, limits Limits) error {
	if width < 1 || height < 1 || width > limits.MaxEdge || height > limits.MaxEdge {
		return ErrDimensions
	}
	if int64(width)*int64(height) > limits.MaxPixels {
		return ErrDimensions
	}
	return nil
}

// acceptVoice stores the container duration when the file has one and it is
// close to the client report. A missing container duration keeps the client
// report only after a small size floor, so a tiny file cannot claim five minutes.
func acceptVoice(parsed, declared, size int, limits Limits) (int, error) {
	if declared < 1 || declared > limits.MaxVoiceMillis || parsed > limits.MaxVoiceMillis {
		return 0, ErrDuration
	}
	if parsed > 0 {
		delta := parsed - declared
		if delta < 0 {
			delta = -delta
		}
		tolerance := parsed / 10
		if tolerance < 2000 {
			tolerance = 2000
		}
		if delta > tolerance {
			return 0, ErrDuration
		}
		return parsed, nil
	}
	floor := declared / 125
	if floor < 32 {
		floor = 32
	}
	if size < floor {
		return 0, ErrDuration
	}
	return declared, nil
}

func durationWebM(data []byte) (int, error) {
	ms, missing, err := parseWebM(data)
	if err != nil {
		return 0, err
	}
	if missing {
		return 0, nil
	}
	return ms, nil
}

func keepGIF(data []byte) ([]byte, error) {
	return data, nil
}

func leadingMarkup(data []byte) bool {
	s := data
	if bytes.HasPrefix(s, []byte{0xEF, 0xBB, 0xBF}) {
		s = s[3:]
	}
	s = bytes.TrimLeft(s, " \t\r\n\f")
	if len(s) == 0 || s[0] != '<' {
		return false
	}
	head := bytes.ToLower(s[:min(16, len(s))])
	for _, prefix := range []string{"<svg", "<!doctype", "<html", "<script", "<head", "<body", "<?xml", "<math"} {
		if bytes.HasPrefix(head, []byte(prefix)) {
			return true
		}
	}
	return false
}

func isJPEG(data []byte) bool {
	return len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF
}
func isPNG(data []byte) bool { return bytes.HasPrefix(data, pngSignature) }
func isGIF(data []byte) bool {
	return bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
}
func isWebP(data []byte) bool {
	return len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}
func isWebM(data []byte) bool {
	return len(data) >= 4 && data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3
}
func isOgg(data []byte) bool { return bytes.HasPrefix(data, []byte("OggS")) }
func isMP4(data []byte) bool {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return false
	}
	return true
}
