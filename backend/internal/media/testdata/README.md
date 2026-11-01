# WebM regression fixture

`location-opus.webm` is a one-second synthetic 440Hz sine wave, encoded with
FFmpeg as Opus/WebM. It contains the artificial location `+25.0000+121.0000/`,
Tags, SeekHead and Cues. It contains no recording or private location data.

Generation:

```text
ffmpeg -f lavfi -i sine=frequency=440:duration=1 -c:a libopus -metadata location=+25.0000+121.0000/ location-opus.webm
```

The tests check removal of coordinates, unchanged byte positions, real decoder
acceptance, unknown-size Segment handling and rejection of malformed tails.
