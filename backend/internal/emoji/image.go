package emoji

import (
	"bytes"
	"encoding/binary"

	"odysseia.local/backend/internal/media"
)

// Prepare sniffs png, apng, gif, and webp, strips the same metadata as other
// images, and sets Animated from the container. JPEG, audio, and markup are
// rejected. There is no resize or transcode.
func Prepare(data []byte, limits Limits) (Image, error) {
	if err := limits.Valid(); err != nil {
		return Image{}, err
	}
	prepared, err := media.Inspect(data, 0, media.Limits{
		MaxBytes: limits.MaxBytes, MaxEdge: limits.MaxEdge,
		MaxPixels: int64(limits.MaxEdge) * int64(limits.MaxEdge), MaxVoiceMillis: 1,
	})
	if err != nil {
		return Image{}, err
	}
	switch prepared.ContentType {
	case "image/png", "image/gif", "image/webp":
	default:
		return Image{}, media.ErrType
	}
	motion, err := animated(prepared.ContentType, prepared.Bytes)
	if err != nil {
		return Image{}, err
	}
	key := ObjectKey(prepared.Sum, prepared.ContentType)
	if key == "" {
		return Image{}, media.ErrType
	}
	return Image{
		ContentType: prepared.ContentType, Bytes: prepared.Bytes,
		Width: prepared.Width, Height: prepared.Height, Animated: motion,
		Sum: prepared.Sum, Key: key,
	}, nil
}

func animated(contentType string, data []byte) (bool, error) {
	switch contentType {
	case "image/gif":
		frames, err := gifFrames(data)
		return frames > 1, err
	case "image/png":
		frames, err := pngFrames(data)
		return frames > 1, err
	case "image/webp":
		return webpAnimated(data)
	default:
		return false, media.ErrType
	}
}

func gifFrames(data []byte) (int, error) {
	if len(data) < 13 || !bytes.HasPrefix(data, []byte("GIF8")) {
		return 0, media.ErrCorrupt
	}
	header := 13
	if data[10]&0x80 != 0 {
		header += 3 << (int(data[10]&7) + 1)
	}
	if header > len(data) {
		return 0, media.ErrCorrupt
	}
	frames := 0
	i := header
	for i < len(data) {
		switch data[i] {
		case 0x3B:
			if frames == 0 {
				return 0, media.ErrCorrupt
			}
			return frames, nil
		case 0x2C:
			next, err := skipGIFImage(data, i)
			if err != nil {
				return 0, err
			}
			frames++
			i = next
		case 0x21:
			if i+2 >= len(data) {
				return 0, media.ErrCorrupt
			}
			next, err := skipGIFSubBlocks(data, i+2)
			if err != nil {
				return 0, err
			}
			i = next
		default:
			return 0, media.ErrCorrupt
		}
	}
	return 0, media.ErrCorrupt
}

func skipGIFImage(data []byte, i int) (int, error) {
	if i+10 > len(data) {
		return 0, media.ErrCorrupt
	}
	end := i + 10
	if data[i+9]&0x80 != 0 {
		end += 3 << (int(data[i+9]&7) + 1)
	}
	if end >= len(data) {
		return 0, media.ErrCorrupt
	}
	return skipGIFSubBlocks(data, end+1)
}

func skipGIFSubBlocks(data []byte, i int) (int, error) {
	for {
		if i >= len(data) {
			return 0, media.ErrCorrupt
		}
		size := int(data[i])
		i++
		if size == 0 {
			return i, nil
		}
		if i+size > len(data) {
			return 0, media.ErrCorrupt
		}
		i += size
	}
}

func pngFrames(data []byte) (int, error) {
	signature := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	if len(data) < len(signature) || !bytes.Equal(data[:len(signature)], signature) {
		return 0, media.ErrCorrupt
	}
	frames := 1
	i := len(signature)
	for i+8 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[i : i+4]))
		if n < 0 || i+12+n > len(data) {
			return 0, media.ErrCorrupt
		}
		kind := string(data[i+4 : i+8])
		payload := data[i+8 : i+8+n]
		if kind == "acTL" {
			if n != 8 {
				return 0, media.ErrCorrupt
			}
			frames = int(binary.BigEndian.Uint32(payload[:4]))
			if frames < 1 {
				return 0, media.ErrCorrupt
			}
		}
		i += 12 + n
		if kind == "IEND" {
			return frames, nil
		}
	}
	return 0, media.ErrCorrupt
}

// webpAnimated reads the VP8X animation bit. The RIFF container numbers bits
// MSB 0: in the first flags byte, Animation is the bit whose value is 0x02.
// A file without VP8X is a still image.
func webpAnimated(data []byte) (bool, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return false, media.ErrCorrupt
	}
	i := 12
	for i+8 <= len(data) {
		id := string(data[i : i+4])
		size := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
		if size < 0 || i+8+size > len(data) {
			return false, media.ErrCorrupt
		}
		if id == "VP8X" {
			if size != 10 {
				return false, media.ErrCorrupt
			}
			return data[i+8]&0x02 != 0, nil
		}
		i += 8 + size
		if size%2 == 1 {
			i++
		}
	}
	return false, nil
}
