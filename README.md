
# webmframe

[![Go Reference](https://pkg.go.dev/badge/github.com/kGoryainov/webmframe.svg)](https://pkg.go.dev/github.com/kGoryainov/webmframe)

`webmframe` extracts codec frames (for example Opus or VP8/VP9 packets) from
WebM/Matroska streams. It is written for the standard library only and has no dependencies.

It reads the container sequentially and does **not** need `SeekHead`, `Cues`,
or known element sizes, so it also works with live recordings
(MediaRecorder, WebRTC capture, etc.), where those elements are usually absent
and `Segment` and `Cluster` have an unknown size.

### Installation

```
$ go get github.com/kGoryainov/webmframe
```

## Usage

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/kGoryainov/webmframe"
)

func main() {
	f, err := os.Open("input.webm")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	reader, err := webmframe.NewReader(f)
	if err != nil {
		log.Fatal(err)
	}

	tracks, err := reader.GetTracks()
	if err != nil {
		log.Fatal(err)
	}
    
	track := tracks[0] // TrackNumber of the track you need

	err = reader.Stream(context.Background(), &track.TrackNumber, func(fr webmframe.Frame) error {
		// fr.Data is one codec frame, e.g. an Opus packet.
		// Return an error to stop reading.
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}
```
`GetTracks` returns all exist tracks in file
`Stream` calls the callback for every frame of the requested track(or for all tracks if no parameter is specified), in file order.


**Parser expects Info before Tracks and Tracks before Cluster,
which is the common order in live recordings. Files with a different order may not parse correctly.**

## Frame

```go
type Frame struct {
	Track uint64 // track number from the block header
	Data  []byte // codec frame; safe to keep after the callback returns(allocate self mem)

	ClusterTimestamp uint64 // Cluster\Timestamp, in Segment Ticks
	BlockTimestamp   int16  // signed offset from the block header, in Track Ticks
	HasTimestamp     bool   // false for 2nd and later frames of a laced block

	KeyFrame bool
}
```

### Frame timestamp calculating

```go
    timestamp, flag := reader.BlockTimestamp(Frame,TrackEntry)
```

`BlockTimestamp` works for all frames that have `Frame.HasTimestamp` otherwise the flag will return false and 0 timestamp 

## Options 

You also can control size limits of reading blocks by default it’s set **4KB**  

Example:
```go
    reader, err := webmframe.NewReader(f, webmframe.WithBlockLim(1 << 26))
	if err != nil {
        log.Fatal(err)
    }
```

## Specification

[RFC 9559: Matroska Media Container Format](https://www.rfc-editor.org/rfc/rfc9559.html)

## License

MIT