package media

import "encoding/binary"

// stripJPEG copies a JPEG and drops XMP, Photoshop IRB, comments, and EXIF
// except a rebuilt Orientation tag. GPS lives in the dropped EXIF. The entropy
// scan is not decoded, so this does not expand pixels and it keeps progressive scans.
func stripJPEG(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, ErrCorrupt
	}
	orientation := jpegOrientation(data)
	out := make([]byte, 0, len(data))
	out = append(out, 0xFF, 0xD8)
	if orientation >= 1 && orientation <= 8 {
		out = append(out, minimalOrientationSegment(byte(orientation))...)
	}
	i := 2
	for i < len(data) {
		if data[i] != 0xFF {
			return nil, ErrCorrupt
		}
		for i < len(data) && data[i] == 0xFF {
			i++
		}
		if i >= len(data) {
			return nil, ErrCorrupt
		}
		marker := data[i]
		i++
		if marker == 0xD9 {
			return append(out, 0xFF, 0xD9), nil
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			out = append(out, 0xFF, marker)
			continue
		}
		if marker == 0x00 || i+2 > len(data) {
			return nil, ErrCorrupt
		}
		seglen := int(data[i])<<8 | int(data[i+1])
		if seglen < 2 || i+seglen > len(data) {
			return nil, ErrCorrupt
		}
		segment := data[i : i+seglen]
		i += seglen
		if marker == 0xDA {
			out = append(out, 0xFF, marker)
			out = append(out, segment...)
			next, err := consumeScan(&out, data, i)
			if err != nil {
				return nil, err
			}
			i = next
			continue
		}
		if marker == 0xFE || marker == 0xE1 || marker == 0xED {
			continue
		}
		out = append(out, 0xFF, marker)
		out = append(out, segment...)
	}
	return nil, ErrCorrupt
}

// consumeScan copies entropy-coded data, including stuffed FF00 and restart
// markers, and stops at the next real marker without consuming it.
func consumeScan(out *[]byte, data []byte, i int) (int, error) {
	start := i
	for i < len(data) {
		if data[i] != 0xFF {
			i++
			continue
		}
		j := i + 1
		for j < len(data) && data[j] == 0xFF {
			j++
		}
		if j >= len(data) {
			return 0, ErrCorrupt
		}
		next := data[j]
		if next == 0x00 || (next >= 0xD0 && next <= 0xD7) {
			i = j + 1
			continue
		}
		*out = append(*out, data[start:i]...)
		return i, nil
	}
	return 0, ErrCorrupt
}

// jpegOrientation reads the first IFD0 Orientation value and ignores GPS and
// other subdirectories. Zero means the file has no usable tag.
func jpegOrientation(data []byte) int {
	i := 2
	for i+4 <= len(data) && data[i] == 0xFF {
		for i < len(data) && data[i] == 0xFF {
			i++
		}
		if i >= len(data) {
			return 0
		}
		marker := data[i]
		i++
		if marker == 0xD9 || marker == 0xDA {
			return 0
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if i+2 > len(data) {
			return 0
		}
		seglen := int(data[i])<<8 | int(data[i+1])
		if seglen < 2 || i+seglen > len(data) {
			return 0
		}
		if marker == 0xE1 && seglen >= 8 && string(data[i+2:i+8]) == "Exif\x00\x00" {
			if value := tiffOrientation(data[i+8 : i+seglen]); value >= 1 && value <= 8 {
				return value
			}
		}
		i += seglen
	}
	return 0
}

func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	le := string(tiff[:2]) == "II"
	if !le && string(tiff[:2]) != "MM" {
		return 0
	}
	if u16(tiff[2:4], le) != 42 {
		return 0
	}
	offset := int(u32(tiff[4:8], le))
	if offset < 8 || offset+2 > len(tiff) {
		return 0
	}
	count := int(u16(tiff[offset:offset+2], le))
	if count < 0 || count > 64 {
		return 0
	}
	entry := offset + 2
	for range count {
		if entry+12 > len(tiff) {
			return 0
		}
		if u16(tiff[entry:entry+2], le) == 0x0112 && u16(tiff[entry+2:entry+4], le) == 3 {
			return int(u16(tiff[entry+8:entry+10], le))
		}
		entry += 12
	}
	return 0
}

func u16(b []byte, le bool) uint16 {
	if le {
		return binary.LittleEndian.Uint16(b)
	}
	return binary.BigEndian.Uint16(b)
}

func u32(b []byte, le bool) uint32 {
	if le {
		return binary.LittleEndian.Uint32(b)
	}
	return binary.BigEndian.Uint32(b)
}

func minimalOrientationSegment(orientation byte) []byte {
	payload := []byte("Exif\x00\x00" +
		"II\x2a\x00" +
		"\x08\x00\x00\x00" +
		"\x01\x00" +
		"\x12\x01\x03\x00\x01\x00\x00\x00")
	payload = append(payload, orientation, 0, 0, 0, 0, 0, 0, 0)
	segment := make([]byte, 4+len(payload))
	segment[0], segment[1] = 0xFF, 0xE1
	binary.BigEndian.PutUint16(segment[2:4], uint16(2+len(payload)))
	copy(segment[4:], payload)
	return segment
}

// displaySize swaps the coded size for EXIF orientations that rotate by 90 degrees.
func displaySize(width, height, orientation int) (int, int) {
	if orientation >= 5 && orientation <= 8 {
		return height, width
	}
	return width, height
}
