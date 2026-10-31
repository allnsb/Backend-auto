package media

// stripJPEG copies a JPEG and drops the containers that carry EXIF, XMP, and
// Photoshop IRB metadata. GPS lives in APP1. The entropy scan is not decoded,
// so this does not expand pixels and it keeps progressive scans intact.
func stripJPEG(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, ErrCorrupt
	}
	out := make([]byte, 0, len(data))
	out = append(out, 0xFF, 0xD8)
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
