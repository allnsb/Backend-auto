package media

// stripGIF drops comment extensions and XMP application blocks. Image data and
// graphic-control extensions stay, so animation is unchanged.
func stripGIF(data []byte) ([]byte, error) {
	if len(data) < 13 || !isGIF(data) {
		return nil, ErrCorrupt
	}
	header := 13
	if data[10]&0x80 != 0 {
		header += 3 << (int(data[10]&7) + 1)
	}
	if header > len(data) {
		return nil, ErrCorrupt
	}
	out := append([]byte(nil), data[:header]...)
	i := header
	for i < len(data) {
		switch data[i] {
		case 0x3B:
			return append(out, 0x3B), nil
		case 0x2C:
			next, err := copyGIFImage(data, i, &out)
			if err != nil {
				return nil, err
			}
			i = next
		case 0x21:
			if i+1 >= len(data) {
				return nil, ErrCorrupt
			}
			if data[i+1] == 0xFE || gifXMP(data, i) {
				next, err := skipGIFBlock(data, i+2)
				if err != nil {
					return nil, err
				}
				i = next
				continue
			}
			next, err := copyGIFExtension(data, i, &out)
			if err != nil {
				return nil, err
			}
			i = next
		default:
			return nil, ErrCorrupt
		}
	}
	return nil, ErrCorrupt
}

func gifXMP(data []byte, i int) bool {
	if i+2 >= len(data) || data[i+1] != 0xFF || data[i+2] != 11 || i+14 > len(data) {
		return false
	}
	id := string(data[i+3 : i+11])
	return id == "XMP Data" || id == "XMP\x00\x00\x00\x00\x00"
}

func copyGIFImage(data []byte, i int, out *[]byte) (int, error) {
	if i+10 > len(data) {
		return 0, ErrCorrupt
	}
	end := i + 10
	if data[i+9]&0x80 != 0 {
		end += 3 << (int(data[i+9]&7) + 1)
	}
	if end >= len(data) {
		return 0, ErrCorrupt
	}
	end++ // LZW minimum code size
	next, err := skipGIFBlock(data, end)
	if err != nil {
		return 0, err
	}
	*out = append(*out, data[i:next]...)
	return next, nil
}

func copyGIFExtension(data []byte, i int, out *[]byte) (int, error) {
	if i+2 >= len(data) {
		return 0, ErrCorrupt
	}
	next, err := skipGIFBlock(data, i+2)
	if err != nil {
		return 0, err
	}
	*out = append(*out, data[i:next]...)
	return next, nil
}

func skipGIFBlock(data []byte, i int) (int, error) {
	for {
		if i >= len(data) {
			return 0, ErrCorrupt
		}
		size := int(data[i])
		i++
		if size == 0 {
			return i, nil
		}
		if i+size > len(data) {
			return 0, ErrCorrupt
		}
		i += size
	}
}
