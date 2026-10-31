package media

import "encoding/binary"

// WebP feature flags in the first payload byte of VP8X, MSB-first as in the
// RIFF container spec: EXIF is bit 3, XMP is bit 2.
const (
	webpEXIF = 1 << 3
	webpXMP  = 1 << 2
)

// stripWebP removes EXIF and XMP chunks and clears the matching VP8X flags.
// The RIFF size is rewritten. Image bitstream chunks are not decoded.
func stripWebP(data []byte) ([]byte, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, ErrCorrupt
	}
	out := make([]byte, 12)
	copy(out, data[:12])
	i := 12
	for i < len(data) {
		if i+8 > len(data) {
			return nil, ErrCorrupt
		}
		id := string(data[i : i+4])
		size := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
		if size < 0 || i+8+size > len(data) {
			return nil, ErrCorrupt
		}
		end := i + 8 + size
		if size%2 == 1 {
			if end >= len(data) {
				return nil, ErrCorrupt
			}
			end++
		}
		if id == "EXIF" || id == "XMP " {
			i = end
			continue
		}
		chunk := data[i:end]
		if id == "VP8X" && size == 10 {
			chunk = append([]byte(nil), chunk...)
			chunk[8] &^= webpEXIF | webpXMP
		}
		out = append(out, chunk...)
		i = end
	}
	if len(out) < 12 {
		return nil, ErrCorrupt
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out, nil
}
