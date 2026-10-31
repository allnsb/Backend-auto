// Package media checks conversation image and voice bytes before they are stored.
// It does not decode pixels or audio samples. Callers keep the authoritative copy.
package media

import "errors"

// Product ceilings. Configuration may only lower a limit; the database checks
// use these same ceilings so a forged row cannot exceed them.
const (
	MaxBytesCeiling       int64 = 10 << 20
	MaxEdgeCeiling              = 32768
	MaxPixelsCeiling      int64 = 67_108_864
	MaxVoiceMillisCeiling       = 300_000
)

var (
	ErrType       = errors.New("media type is not allowed")
	ErrTooLarge   = errors.New("media exceeds the size limit")
	ErrDimensions = errors.New("image dimensions are not allowed")
	ErrDuration   = errors.New("audio duration is not allowed")
	ErrCorrupt    = errors.New("media bytes are not valid")
)

// Limits is the process allowlist. Zero-value fields are not implicit defaults.
type Limits struct {
	MaxBytes       int64
	MaxEdge        int
	MaxPixels      int64
	MaxVoiceMillis int
}

// DefaultLimits is the documented P4-b budget: 10MiB, 8192px edge, 16 megapixels, 5 minutes.
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:       MaxBytesCeiling,
		MaxEdge:        8192,
		MaxPixels:      16_777_216,
		MaxVoiceMillis: MaxVoiceMillisCeiling,
	}
}

// Valid rejects a limit above the database ceiling or below one unit.
func (l Limits) Valid() error {
	if l.MaxBytes < 1 || l.MaxBytes > MaxBytesCeiling {
		return errors.New("attachment byte limit must be from 1 to 10MiB")
	}
	if l.MaxEdge < 1 || l.MaxEdge > MaxEdgeCeiling {
		return errors.New("image edge limit must be from 1 to 32768")
	}
	if l.MaxPixels < 1 || l.MaxPixels > MaxPixelsCeiling {
		return errors.New("image pixel limit must be from 1 to 67108864")
	}
	if l.MaxVoiceMillis < 1 || l.MaxVoiceMillis > MaxVoiceMillisCeiling {
		return errors.New("voice duration limit must be from 1 to 300000 milliseconds")
	}
	return nil
}
