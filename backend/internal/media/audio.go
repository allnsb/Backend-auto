package media

import (
	"encoding/binary"
	"errors"
	"math"
)

// parseWebM reports the container duration in milliseconds. missing is true when
// a well-formed Opus-only WebM file has no positive Duration element. Some
// recorders stream the Segment with an unknown size and omit Duration; the
// caller then applies a size floor to the client-reported length.
func parseWebM(data []byte) (durationMS int, missing bool, err error) {
	info := webmInfo{scale: 1_000_000}
	if err := walkEBML(data, 0, &info); err != nil {
		return 0, false, err
	}
	if info.docType != "webm" || info.video || !info.audio || !info.opus {
		return 0, false, ErrType
	}
	if !info.haveDuration {
		return 0, true, nil
	}
	ms := info.duration * float64(info.scale) / 1e6
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 1 || ms > float64(math.MaxInt32) {
		return 0, false, ErrDuration
	}
	return int(math.Round(ms)), false, nil
}

type webmInfo struct {
	docType      string
	scale        uint64
	duration     float64
	haveDuration bool
	audio, video bool
	opus         bool
	elements     int
}

const (
	ebmlIDHeader    = 0x1A45DFA3
	ebmlIDDocType   = 0x4282
	ebmlIDSegment   = 0x18538067
	ebmlIDInfo      = 0x1549A966
	ebmlIDScale     = 0x2AD7B1
	ebmlIDDuration  = 0x4489
	ebmlIDTracks    = 0x1654AE6B
	ebmlIDTrack     = 0xAE
	ebmlIDTrackType = 0x83
	ebmlIDCodec     = 0x86
	ebmlIDCluster   = 0x1F43B675
)

func walkEBML(data []byte, depth int, info *webmInfo) error {
	if depth > 6 {
		return ErrCorrupt
	}
	i := 0
	for i < len(data) {
		info.elements++
		if info.elements > 10000 {
			return ErrCorrupt
		}
		id, width, err := readEBMLID(data, i)
		if err != nil {
			return err
		}
		i += width
		size, unknown, width, err := readEBMLSize(data, i)
		if err != nil {
			return err
		}
		i += width
		end := len(data)
		if !unknown {
			if size > uint64(len(data)-i) {
				return ErrCorrupt
			}
			end = i + int(size)
		} else if id != ebmlIDSegment && id != ebmlIDCluster {
			return ErrCorrupt
		}
		if err := applyEBML(data[i:end], id, unknown, depth, info); err != nil {
			if errors.Is(err, errEBMLDone) {
				return nil
			}
			return err
		}
		if unknown {
			return nil
		}
		i = end
	}
	return nil
}

var errEBMLDone = errors.New("ebml headers are complete")

func applyEBML(payload []byte, id uint64, unknown bool, depth int, info *webmInfo) error {
	switch id {
	case ebmlIDHeader, ebmlIDSegment, ebmlIDInfo, ebmlIDTracks, ebmlIDTrack:
		return walkEBML(payload, depth+1, info)
	case ebmlIDCluster:
		// Samples are not decoded. Headers before the first cluster are enough.
		return errEBMLDone
	case ebmlIDDocType:
		info.docType = string(payload)
	case ebmlIDScale:
		scale, err := readEBMLUint(payload)
		if err != nil || scale == 0 {
			return ErrCorrupt
		}
		info.scale = scale
	case ebmlIDDuration:
		value, err := readEBMLFloat(payload)
		if err != nil {
			return err
		}
		info.duration = value
		info.haveDuration = value > 0
	case ebmlIDTrackType:
		kind, err := readEBMLUint(payload)
		if err != nil {
			return err
		}
		if kind == 1 {
			info.video = true
			return nil
		}
		if kind != 2 {
			return ErrType
		}
		info.audio = true
	case ebmlIDCodec:
		if string(payload) != "A_OPUS" {
			return ErrType
		}
		info.opus = true
	default:
		if unknown {
			return ErrCorrupt
		}
	}
	return nil
}

func readEBMLID(data []byte, i int) (uint64, int, error) {
	value, width, _, err := readEBMLVint(data, i, true)
	return value, width, err
}

func readEBMLSize(data []byte, i int) (uint64, bool, int, error) {
	value, width, unknown, err := readEBMLVint(data, i, false)
	return value, unknown, width, err
}

func readEBMLVint(data []byte, i int, keepMarker bool) (uint64, int, bool, error) {
	if i >= len(data) || data[i] == 0 {
		return 0, 0, false, ErrCorrupt
	}
	first := data[i]
	width := 1
	mask := byte(0x80)
	for first&mask == 0 {
		width++
		mask >>= 1
		if width > 8 {
			return 0, 0, false, ErrCorrupt
		}
	}
	if i+width > len(data) {
		return 0, 0, false, ErrCorrupt
	}
	raw := uint64(first)
	for n := 1; n < width; n++ {
		raw = raw<<8 | uint64(data[i+n])
	}
	if keepMarker {
		return raw, width, false, nil
	}
	raw &^= uint64(mask) << (8 * (width - 1))
	bits := width * 7
	if width == 8 {
		bits = 56
	}
	unknown := bits < 63 && raw == (uint64(1)<<bits)-1
	return raw, width, unknown, nil
}

func readEBMLUint(payload []byte) (uint64, error) {
	if len(payload) == 0 || len(payload) > 8 {
		return 0, ErrCorrupt
	}
	var value uint64
	for _, b := range payload {
		value = value<<8 | uint64(b)
	}
	return value, nil
}

func readEBMLFloat(payload []byte) (float64, error) {
	switch len(payload) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(payload))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(payload)), nil
	default:
		return 0, ErrCorrupt
	}
}

// parseMP4 returns the duration in milliseconds. A video track is rejected.
// A zero movie header is not corrupt: fragmented recordings leave mvhd at 0
// and put the length in mehd or trun. Zero means the caller should use the
// client-reported duration and the size floor.
func parseMP4(data []byte) (int, error) {
	state := mp4State{}
	if err := walkMP4(data, 0, &state); err != nil {
		return 0, err
	}
	if state.video || !state.ftyp || !state.audio {
		return 0, ErrType
	}
	if state.timescale == 0 {
		return 0, ErrCorrupt
	}
	ticks := state.headerTicks
	if ticks == 0 {
		ticks = state.fragmentTicks
	}
	if ticks == 0 {
		ticks = state.sampleTicks
	}
	if ticks == 0 {
		return 0, nil
	}
	ms := ticks * 1000 / uint64(state.timescale)
	if ms < 1 || ms > uint64(math.MaxInt32) {
		return 0, ErrDuration
	}
	return int(ms), nil
}

type mp4State struct {
	ftyp, audio, video bool
	timescale          uint32
	headerTicks        uint64
	fragmentTicks      uint64
	sampleTicks        uint64
	boxes              int
}

func walkMP4(data []byte, depth int, state *mp4State) error {
	if depth > 8 {
		return ErrCorrupt
	}
	i := 0
	for i+8 <= len(data) {
		state.boxes++
		if state.boxes > 10000 {
			return ErrCorrupt
		}
		size := int(binary.BigEndian.Uint32(data[i : i+4]))
		kind := string(data[i+4 : i+8])
		header := 8
		if size == 1 {
			if i+16 > len(data) {
				return ErrCorrupt
			}
			large := binary.BigEndian.Uint64(data[i+8 : i+16])
			if large > uint64(len(data)) || large < 16 {
				return ErrCorrupt
			}
			size = int(large)
			header = 16
		}
		if size == 0 {
			size = len(data) - i
		}
		if size < header || i+size > len(data) {
			return ErrCorrupt
		}
		payload := data[i+header : i+size]
		switch kind {
		case "moov", "trak", "mdia", "minf", "mvex", "moof", "traf":
			if err := walkMP4(payload, depth+1, state); err != nil {
				return err
			}
		case "mvhd":
			timescale, ticks, err := readMVHD(payload)
			if err != nil {
				return err
			}
			if state.timescale != 0 {
				return ErrCorrupt
			}
			state.timescale = timescale
			state.headerTicks = ticks
		case "mehd":
			ticks, err := readMEHD(payload)
			if err != nil {
				return err
			}
			state.fragmentTicks = ticks
		case "trun":
			if err := addTrun(payload, state); err != nil {
				return err
			}
		case "hdlr":
			if len(payload) < 12 {
				return ErrCorrupt
			}
			switch string(payload[8:12]) {
			case "vide":
				state.video = true
			case "soun":
				state.audio = true
			}
		case "ftyp":
			state.ftyp = true
		}
		i += size
	}
	if i != len(data) {
		return ErrCorrupt
	}
	return nil
}

func readMVHD(payload []byte) (uint32, uint64, error) {
	if len(payload) < 20 {
		return 0, 0, ErrCorrupt
	}
	var timescale uint32
	var ticks uint64
	switch payload[0] {
	case 0:
		timescale = binary.BigEndian.Uint32(payload[12:16])
		ticks = uint64(binary.BigEndian.Uint32(payload[16:20]))
	case 1:
		if len(payload) < 32 {
			return 0, 0, ErrCorrupt
		}
		timescale = binary.BigEndian.Uint32(payload[20:24])
		ticks = binary.BigEndian.Uint64(payload[24:32])
	default:
		return 0, 0, ErrCorrupt
	}
	if timescale == 0 {
		return 0, 0, ErrCorrupt
	}
	return timescale, ticks, nil
}

func readMEHD(payload []byte) (uint64, error) {
	if len(payload) < 8 {
		return 0, ErrCorrupt
	}
	if payload[0] == 1 {
		if len(payload) < 12 {
			return 0, ErrCorrupt
		}
		return binary.BigEndian.Uint64(payload[4:12]), nil
	}
	return uint64(binary.BigEndian.Uint32(payload[4:8])), nil
}

func addTrun(payload []byte, state *mp4State) error {
	if len(payload) < 8 {
		return ErrCorrupt
	}
	flags := binary.BigEndian.Uint32(payload[:4]) & 0xFFFFFF
	count := binary.BigEndian.Uint32(payload[4:8])
	if count > 1_000_000 {
		return ErrCorrupt
	}
	pos := 8
	if flags&0x1 != 0 {
		pos += 4
	}
	if flags&0x4 != 0 {
		pos += 4
	}
	for range count {
		if flags&0x100 != 0 {
			if pos+4 > len(payload) {
				return ErrCorrupt
			}
			state.sampleTicks += uint64(binary.BigEndian.Uint32(payload[pos : pos+4]))
			pos += 4
		}
		if flags&0x200 != 0 {
			pos += 4
		}
		if flags&0x400 != 0 {
			pos += 4
		}
		if flags&0x800 != 0 {
			pos += 4
		}
		if pos > len(payload) {
			return ErrCorrupt
		}
	}
	return nil
}

// stripMP4Location drops udta and the ISO 6709 xyz box. Those hold GPS in
// recorder output. Sample tables and the movie header stay intact.
func stripMP4Location(data []byte) ([]byte, error) {
	return rewriteMP4(data, 0)
}

func rewriteMP4(data []byte, depth int) ([]byte, error) {
	if depth > 8 {
		return nil, ErrCorrupt
	}
	out := make([]byte, 0, len(data))
	i := 0
	boxes := 0
	for i+8 <= len(data) {
		boxes++
		if boxes > 10000 {
			return nil, ErrCorrupt
		}
		size := int(binary.BigEndian.Uint32(data[i : i+4]))
		kind := string(data[i+4 : i+8])
		header := 8
		if size == 1 {
			if i+16 > len(data) {
				return nil, ErrCorrupt
			}
			large := binary.BigEndian.Uint64(data[i+8 : i+16])
			if large > uint64(len(data)-i) || large < 16 {
				return nil, ErrCorrupt
			}
			size = int(large)
			header = 16
		}
		if size == 0 {
			size = len(data) - i
		}
		if size < header || i+size > len(data) {
			return nil, ErrCorrupt
		}
		payload := data[i+header : i+size]
		i += size
		if kind == "udta" || kind == "xyz " || kind == "\xa9xyz" {
			continue
		}
		body := payload
		if mp4Container(kind) {
			rewritten, err := rewriteMP4(payload, depth+1)
			if err != nil {
				return nil, err
			}
			body = rewritten
		}
		out = append(out, writeMP4Box(kind, body)...)
	}
	if i != len(data) {
		return nil, ErrCorrupt
	}
	return out, nil
}

func mp4Container(kind string) bool {
	switch kind {
	case "moov", "trak", "mdia", "minf", "stbl", "edts", "mvex", "moof", "traf":
		return true
	default:
		return false
	}
}

func writeMP4Box(kind string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
	copy(out[4:8], kind)
	copy(out[8:], payload)
	return out
}

// stripWebMLocation removes Matroska Tags elements. Voice notes do not need
// them, and recorders put coordinates there. Clusters are copied whole.
func stripWebMLocation(data []byte) []byte {
	cleaned, err := filterEBML(data, 0)
	if err != nil || len(cleaned) == 0 {
		return data
	}
	return cleaned
}

const ebmlIDTags = 0x1254C367

func filterEBML(data []byte, depth int) ([]byte, error) {
	if depth > 6 {
		return nil, ErrCorrupt
	}
	out := make([]byte, 0, len(data))
	i := 0
	elements := 0
	for i < len(data) {
		elements++
		if elements > 10000 {
			return nil, ErrCorrupt
		}
		start := i
		id, width, err := readEBMLID(data, i)
		if err != nil {
			return nil, err
		}
		i += width
		idBytes := append([]byte(nil), data[start:start+width]...)
		size, unknown, sizeWidth, err := readEBMLSize(data, i)
		if err != nil {
			return nil, err
		}
		i += sizeWidth
		if unknown {
			if id == ebmlIDCluster {
				return append(out, data[start:]...), nil
			}
			payload, err := filterEBML(data[i:], depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, idBytes...)
			out = append(out, data[i-sizeWidth:i]...)
			return append(out, payload...), nil
		}
		if size > uint64(len(data)-i) {
			return nil, ErrCorrupt
		}
		end := i + int(size)
		payload := data[i:end]
		i = end
		if id == ebmlIDTags {
			continue
		}
		if ebmlMaster(id) {
			filtered, err := filterEBML(payload, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, writeEBML(idBytes, filtered)...)
			continue
		}
		out = append(out, data[start:end]...)
	}
	return out, nil
}

func ebmlMaster(id uint64) bool {
	switch id {
	case ebmlIDHeader, ebmlIDSegment, ebmlIDInfo, ebmlIDTracks, ebmlIDTrack:
		return true
	default:
		return false
	}
}

func writeEBML(id, payload []byte) []byte {
	size := encodeEBMLSize(len(payload))
	out := make([]byte, 0, len(id)+len(size)+len(payload))
	out = append(out, id...)
	out = append(out, size...)
	return append(out, payload...)
}

func encodeEBMLSize(n int) []byte {
	switch {
	case n < 0x7F:
		return []byte{0x80 | byte(n)}
	case n < 0x3FFF:
		return []byte{0x40 | byte(n>>8), byte(n)}
	case n < 0x1FFFFF:
		return []byte{0x20 | byte(n>>16), byte(n >> 8), byte(n)}
	default:
		return []byte{0x10 | byte(n>>24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
}

// parseOgg returns the duration of a single Opus or Vorbis logical stream.
// The granule position is the container clock. Samples are not decoded, so a
// forged granule can under-report a file that is still bounded by the byte cap.
func parseOgg(data []byte) (int, error) {
	if len(data) < 27 || string(data[:4]) != "OggS" {
		return 0, ErrType
	}
	streams := map[uint32]*oggStream{}
	i := 0
	pages := 0
	for i < len(data) {
		pages++
		if pages > 200000 || i+27 > len(data) || string(data[i:i+4]) != "OggS" || data[i+4] != 0 {
			return 0, ErrCorrupt
		}
		header := data[i+5]
		granule := int64(binary.LittleEndian.Uint64(data[i+6 : i+14]))
		serial := binary.LittleEndian.Uint32(data[i+14 : i+18])
		segments := int(data[i+26])
		if i+27+segments > len(data) {
			return 0, ErrCorrupt
		}
		body := 0
		for _, size := range data[i+27 : i+27+segments] {
			body += int(size)
		}
		start := i + 27 + segments
		if start+body > len(data) {
			return 0, ErrCorrupt
		}
		payload := data[start : start+body]
		i = start + body
		current := streams[serial]
		if current == nil {
			current = &oggStream{}
			streams[serial] = current
		}
		if header&0x02 != 0 {
			if err := classifyOgg(current, payload); err != nil {
				return 0, err
			}
		}
		if granule >= 0 {
			current.last = granule
			current.sawGranule = true
		}
	}
	if len(streams) != 1 {
		return 0, ErrType
	}
	var only *oggStream
	for _, current := range streams {
		only = current
	}
	if only.codec == "" || !only.sawGranule || only.rate == 0 {
		return 0, ErrCorrupt
	}
	samples := only.last
	if only.codec == "opus" {
		if only.last < int64(only.preSkip) {
			return 0, ErrCorrupt
		}
		samples = only.last - int64(only.preSkip)
	}
	if samples < 1 {
		return 0, ErrDuration
	}
	ms := samples * 1000 / int64(only.rate)
	if ms < 1 || ms > math.MaxInt32 {
		return 0, ErrDuration
	}
	return int(ms), nil
}

type oggStream struct {
	codec      string
	preSkip    uint64
	rate       uint32
	last       int64
	sawGranule bool
}

func classifyOgg(current *oggStream, payload []byte) error {
	if len(payload) >= 19 && string(payload[:8]) == "OpusHead" {
		current.codec = "opus"
		current.preSkip = uint64(binary.LittleEndian.Uint16(payload[10:12]))
		current.rate = 48000
		return nil
	}
	if len(payload) >= 16 && payload[0] == 1 && string(payload[1:7]) == "vorbis" {
		current.codec = "vorbis"
		current.rate = binary.LittleEndian.Uint32(payload[12:16])
		if current.rate == 0 {
			return ErrCorrupt
		}
		return nil
	}
	return ErrType
}
