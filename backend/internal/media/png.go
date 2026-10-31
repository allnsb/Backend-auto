package media

import (
	"bytes"
	"encoding/binary"
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// stripPNG drops text and EXIF chunks. Kept chunks are copied with their
// original CRCs. Animation chunks (acTL, fcTL, fdAT) stay so a motion PNG
// is not flattened.
func stripPNG(data []byte) ([]byte, error) {
	if len(data) < len(pngSignature) || !bytes.Equal(data[:len(pngSignature)], pngSignature) {
		return nil, ErrCorrupt
	}
	out := append([]byte(nil), pngSignature...)
	i := len(pngSignature)
	sawIHDR := false
	sawIEND := false
	for i+12 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[i : i+4]))
		if n < 0 || i+12+n > len(data) {
			return nil, ErrCorrupt
		}
		kind := string(data[i+4 : i+8])
		chunk := data[i : i+12+n]
		i += 12 + n
		switch kind {
		case "eXIf", "tEXt", "zTXt", "iTXt", "tIME":
			continue
		case "IHDR":
			sawIHDR = true
		case "IEND":
			sawIEND = true
		}
		out = append(out, chunk...)
		if kind == "IEND" {
			break
		}
	}
	if !sawIHDR || !sawIEND || i != len(data) {
		return nil, ErrCorrupt
	}
	return out, nil
}
