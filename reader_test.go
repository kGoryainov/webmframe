package webmframe

import (
	"context"
	"os"
	"testing"
)

func TestAudioChromeOpusCodecStream(t *testing.T) {
	r, err := NewReader(openFile(t, "samples/opus_test.webm"))
	if err != nil {
		t.Fatal(err)
	}
	stream(t, r)
}

func TestAudioChromeVorbisCodecStream(t *testing.T) {
	r, err := NewReader(openFile(t, "samples/vorbis_test.webm"))
	if err != nil {
		t.Fatal(err)
	}
	stream(t, r)
}

func TestAudioChromeVP9CodecStream(t *testing.T) {
	r, err := NewReader(openFile(t, "samples/vp9_opus_test.webm"))
	if err != nil {
		t.Fatal(err)
	}
	stream(t, r)
}

// file has fixed-size lacing type, file was created by mkvmerge
func TestAudioOpusCodecWithLacingStream(t *testing.T) {
	r, err := NewReader(openFile(t, "samples/opus_lacing_test.webm"))
	if err != nil {
		t.Fatal(err)
	}
	var hasLacing bool
	err = r.Stream(context.Background(), nil, func(frame Frame) error {
		if !frame.HasTimestamp {
			hasLacing = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasLacing {
		t.Error("expected at least one laced frame")
	}
}

func TestParseInfo(t *testing.T) {
	var files = []string{
		"samples/opus_test.webm",
		"samples/opus_lacing_test.webm",
		"samples/vorbis_test.webm",
		"samples/vp9_opus_test.webm",
	}

	for _, file := range files {
		r, err := NewReader(openFile(t, file))
		if err != nil {
			t.Fatal(err)
		}
		if r.timeScale == 0 {
			t.Fatal("timeScale must be greater than 0")
		}
		t.Log(r.timeScale)
	}
}

func TestGetTracks(t *testing.T) {
	var tests = []struct {
		file           string
		numTrackExpect int
	}{
		{"samples/opus_test.webm", 1},
		{"samples/opus_lacing_test.webm", 1},
		{"samples/vorbis_test.webm", 1},
		{"samples/vp9_opus_test.webm", 2},
	}
	for _, test := range tests {
		r, err := NewReader(openFile(t, test.file))
		if err != nil {
			t.Fatal(err)
		}
		tracks, err := r.GetTracks()
		if err != nil {
			t.Fatal(err)
		}
		if len(tracks) != test.numTrackExpect {
			t.Errorf("got %d tracks, want %d", len(tracks), test.numTrackExpect)
		}
		for _, track := range tracks {
			if track.TrackNumber == 0 {
				t.Errorf("got empty track number")
			}
		}
	}
}

func openFile(t *testing.T, name string) *os.File {
	t.Helper()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}
func stream(t *testing.T, r *Reader) {
	count := 0
	err := r.Stream(context.Background(), nil, func(frame Frame) error {
		count++
		if len(frame.Data) == 0 {
			t.Errorf("empty frame data")
		}
		return nil
	})
	if count == 0 {
		t.Fatal("no frames emitted")
	}
	if err != nil {
		t.Fatal(err)
	}
}
