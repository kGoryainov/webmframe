package webmframe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
)

// Option using for NewReader
type Option func(*Reader)

func WithBlockLim(lim uint64) func(*Reader) {
	return func(r *Reader) {
		if lim > 0 {
			r.blockSizeLim = lim
		}
	}
}

// Reader
type Reader struct {
	br           *bufio.Reader
	timeScale    uint64
	blockSizeLim uint64
}

// NewReader
func NewReader(r io.Reader, opt ...Option) (*Reader, error) {
	rd := &Reader{
		br:           bufio.NewReaderSize(r, 64<<10),
		timeScale:    1_000_000,
		blockSizeLim: defBodyLim,
	}
	if err := rd.parseInfo(); err != nil {
		return nil, err
	}
	for _, o := range opt {
		o(rd)
	}
	return rd, nil
}

// BlockTimestamp return nanosecond timestamp
// ( ( Cluster\Timestamp + ( block timestamp * TrackTimestampScale ) ) * TimestampScale ) - CodecDelay
// This function cannot calculate timestamp for laced frames as stated in the specification:
// "In the lace, each frame after the first one has an underdetermined timestamp."
// For Opus, this means you need to read the sample duration from the TOC byte.
func (r *Reader) BlockTimestamp(f Frame, t TrackEntry) (int64, bool) {
	if !f.HasTimestamp {
		return 0, false
	}
	trackScale := t.TimeScale
	if trackScale == 0 {
		trackScale = 1.0
	}
	ticks := float64(f.ClusterTimestamp) + float64(f.BlockTimestamp)*trackScale

	ns := ticks * float64(r.timeScale)

	return int64(ns) - int64(t.CodecDelay), true
}

// Stream written in accordance with RFC https://ftp.nic.ad.jp/in-notes/rfc9559.html
func (r *Reader) Stream(ctx context.Context, track *uint64, emit func(Frame) error) error {
	var clusterTime uint64
	for {
		head, err := readHeader(r.br)
		if err != nil {
			return catchEOF(err)
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		switch head.id {
		case 0x1F43B675: // Segment, Cluster ID,
			continue
			// SimpleBlock - A3,  BlockGroup - A0, TimeStamp - E7
		case 0xA3, 0xA0, 0xE7:
		default: // skip other elements
			if head.unknown {
				return errors.New("unknown size on non-container element")
			}
			if _, err = r.br.Discard(int(head.size)); err != nil {
				return catchEOF(err)
			}
			continue
		}
		if err = head.isBad(r.blockSizeLim); err != nil {
			return err
		}
		body := make([]byte, head.size)
		if _, err = io.ReadFull(r.br, body); err != nil {
			return catchEOF(err)
		}

		var frames []Frame
		switch head.id {
		case 0xE7:
			clusterTime = readUint(body)
		case 0xA3: // SimpleBlock (ID 0xA3)
			frames, err = parseBlock(body, clusterTime, true, false)
		case 0xA0: //  BlockGroup (EBML element)
			var block []byte
			var hasRef bool
			err = walk(body, func(cid uint32, cb []byte) {
				// path \Segment\Cluster\BlockGroup\Block
				// main Block witch references on SimpleBlock
				if cid == 0xA1 {
					block = cb
				}
				if cid == 0xFB {
					hasRef = true
				}
			})
			if block != nil && err == nil {
				frames, err = parseBlock(block, clusterTime, false, hasRef)
			}
		}
		if err != nil {
			return err
		}
		for _, f := range frames {
			if track == nil || f.Track == *track {
				if err = emit(f); err != nil {
					return err
				}
			}
		}
	}
}

// readVID extract element id, with the remain-marker bit
func readVID(br *bufio.Reader) (uint32, error) {
	b, err := br.ReadByte()
	if err != nil {
		return 0, err
	}
	if b == 0 {
		return 0, errors.New("invalid VINT: zero byte")
	}
	n := bits.LeadingZeros8(b) + 1
	if n > 4 {
		return 0, errors.New("invalid VINT: can not be more than 4 bytes")
	}

	// first byte is - [leading zeros][marker 1][bits of a value]
	id := uint32(b)
	for i := 1; i < n; i++ {
		c, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		id = id<<8 | uint32(c)
	}
	return id, nil
}

func readSize(br *bufio.Reader) (size uint64, unknown bool, err error) {
	b, err := br.ReadByte()
	if err != nil {
		return
	}
	n := bits.LeadingZeros8(b) + 1
	if n > 8 {
		err = errors.New("bad size")
		return
	}
	size = uint64(b) & (0xFF >> n)

	// take leading zeros in account
	unknown = size == uint64(0xFF>>n)

	for i := 1; i < n; i++ {
		var c byte
		if c, err = br.ReadByte(); err != nil {
			return
		}
		if c != 0xFF {
			unknown = false
		}
		size = size<<8 | uint64(c)
	}
	return
}

// Frame
type Frame struct {
	Track uint64
	Data  []byte

	ClusterTimestamp uint64 // Cluster\Timestamp, Segment Ticks
	BlockTimestamp   int16  // Block timestamp , Track Ticks
	HasTimestamp     bool

	KeyFrame bool
}

// parseBlock
//
//	0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |               |                               |K|     |I|LAC|D|
// |  Track Number |         Timestamp             |E|Rsvrd|N|ING|I|
// |               |                               |Y|     |V|   |S|
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// Track Number:
// An EBML VINT-coded track number.
// Timestamp:
// 16 bits. Signed timestamp in Track Ticks.
// KEY:
// 1 bit. Keyframe; Set when the Block contains only keyframes.
// Rsvrd:
// 3 bits. Reserved bits MUST be set to 0.
// INV:
// 1 bit. Invisible; the codec SHOULD decode this frame but not display it.
// LACING:
// 2 bits. Uses lacing mode.
// DIS:
// 1 bit. Discardable; The frames of the Block can be discarded during playing if needed.
func parseBlock(raw []byte, clusterTS uint64, simpleBlock bool, hasRef bool) ([]Frame, error) {
	track, n, err := readVint(raw)
	if err != nil {
		return nil, err
	}
	raw = raw[n:]
	if len(raw) < 3 {
		return nil, errors.New("block too short")
	}
	//The Block element and SimpleBlock element store their timestamps as 16-bit signed integers,
	timestamp := int16(raw[0])<<8 | int16(raw[1])

	flags := raw[2]

	// keyframe flag MUST NOT be set in the SimpleBlock or a ReferenceBlock MUST be used to signify the frame is not a RAP.
	keyFrame := !hasRef
	if simpleBlock {
		keyFrame = (flags & 0x80) > 0
	}

	payload := raw[3:]

	//Lacing is a mechanism to save space when storing data. It is typically used for small blocks of data
	// It packs multiple frames into a single Block or SimpleBlock.
	lacing := (flags >> 1) & 0x03

	if lacing == 0 {
		return []Frame{{Track: track, Data: payload,
			KeyFrame: keyFrame, ClusterTimestamp: clusterTS, BlockTimestamp: timestamp, HasTimestamp: true}}, nil
	}

	sizes, rest, err := parseLaceSizes(payload, lacing)
	if err != nil {
		return nil, err
	}
	frames := make([]Frame, 0, len(sizes))
	for i, s := range sizes {
		if len(rest) < s {
			return nil, errors.New("lace overflow")
		}
		//A Block only contains a single timestamp value.
		//But when lacing is used, it contains more than one frame.
		//Each frame originally has its own timestamp, or Presentation Timestamp (PTS).
		//That timestamp applies to the first frame in the lace.
		if i == 0 {
			frames = append(frames, Frame{Track: track,
				Data: rest[:s], KeyFrame: keyFrame, ClusterTimestamp: clusterTS, BlockTimestamp: timestamp, HasTimestamp: true})
		} else {
			frames = append(frames, Frame{Track: track,
				Data: rest[:s], KeyFrame: keyFrame, ClusterTimestamp: clusterTS, HasTimestamp: false})
		}
		rest = rest[s:]
	}
	return frames, nil
}

// parseLaceSizes
// // 00b:
// // no lacing (Section 10.3.1)
// // 01b:
// // Xiph lacing (Section 10.3.2)
// // 11b:
// // EBML lacing (Section 10.3.3)
// // 10b:
// // fixed-size lacing (Section 10.3.4)
func parseLaceSizes(p []byte, lacing byte) (sizes []int, rest []byte, err error) {
	if len(p) < 1 {
		return nil, nil, errors.New("no lace header")
	}
	count := int(p[0]) + 1
	p = p[1:]
	sizes = make([]int, count)

	switch lacing {
	case 0x01: // Xiph
		total := 0
		for i := 0; i < count-1; i++ {
			s := 0
			for {
				if len(p) == 0 {
					return nil, nil, errors.New("xiph lace eof")
				}
				b := p[0]
				p = p[1:]
				s += int(b)
				if b != 255 {
					break
				}
			}
			sizes[i] = s
			total += s
		}
		sizes[count-1] = len(p) - total

	case 0x03: // EBML
		first, n, err := readVint(p)
		if err != nil {
			return nil, nil, err
		}
		p = p[n:]
		sizes[0] = int(first)
		total := sizes[0]
		prev := int64(first)
		for i := 1; i < count-1; i++ {
			v, n, err := readVint(p)
			if err != nil {
				return nil, nil, err
			}
			p = p[n:]
			delta := int64(v) - (int64(1)<<(uint(7*n)-1) - 1) // vint
			prev += delta
			sizes[i] = int(prev)
			total += sizes[i]
		}
		sizes[count-1] = len(p) - total

	case 0x02: // fixed-size
		each := len(p) / count
		for i := range sizes {
			sizes[i] = each
		}

	default:
		return nil, nil, fmt.Errorf("unknown lacing %d", lacing)
	}

	for _, s := range sizes {
		if s < 0 {
			return nil, nil, errors.New("negative lace size")
		}
	}
	return sizes, p, nil
}

func readVint(b []byte) (val uint64, n int, err error) {
	if len(b) == 0 {
		return 0, 0, errors.New("empty vint")
	}
	first := b[0]
	if first == 0 {
		return 0, 0, errors.New("invalid vint: zero byte")
	}
	n = 1
	// количество байт числа, расчитывается из (count lead zeros + 1)
	z := bits.LeadingZeros8(first) + 1
	if len(b) < z {
		return 0, 0, io.ErrUnexpectedEOF
	}

	// первый байт это - [ведущие нули][маркерная 1][старшие биты значения]
	// 0xFF - 256, сдвигаем биты b[0] на n + (1 маркерный байт)
	// чтобы остались старшие биты
	val = uint64(first) & (0xFF >> z)
	for ; n < z; n++ {
		c := b[n]
		val = val<<8 | uint64(c)
	}
	return val, n, nil
}

// readIDBytes equaling readVID but works with []byte
func readIDBytes(b []byte) (id uint32, n int, err error) {
	if len(b) == 0 {
		return 0, 0, errors.New("empty id")
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if n > 4 || len(b) < n {
		return 0, 0, errors.New("bad id")
	}
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(b[i])
	}
	return id, n, nil
}

// walk calling fn for every child elements of embl specific body.
func walk(b []byte, fn func(id uint32, body []byte)) error {
	for len(b) > 0 {
		id, n, err := readIDBytes(b)
		if err != nil {
			return err
		}
		b = b[n:]

		size, n, err := readVint(b)
		if err != nil {
			return err
		}
		b = b[n:]

		if uint64(len(b)) < size {
			return errors.New("element overflows parent")
		}
		fn(id, b[:size])
		b = b[size:]
	}
	return nil
}

type header struct {
	id      uint32
	size    uint64
	unknown bool
}

const defBodyLim = 1 << 20

func (h *header) isBad(limSize uint64) error {
	if h.unknown || h.size > limSize {
		return errors.New("bad element size")
	}
	return nil
}

func readHeader(r *bufio.Reader) (header, error) {
	id, err := readVID(r)
	if err != nil {
		return header{}, err
	}
	size, unknown, err := readSize(r)
	if err != nil {
		return header{}, err
	}
	return header{size: size, id: id, unknown: unknown}, nil
}

func (r *Reader) GetTracks() ([]TrackEntry, error) {
	for {
		head, err := readHeader(r.br)
		if err != nil {
			return nil, err
		}
		switch head.id {
		case 0x1654AE6B:
		default:
			if head.unknown {
				return nil, errors.New("unknown size on non-container element")
			}
			if _, err = r.br.Discard(int(head.size)); err != nil {
				return nil, err
			}
			continue
		}
		if err = head.isBad(defBodyLim); err != nil {
			return nil, err
		}
		var body = make([]byte, head.size)
		if _, err = io.ReadFull(r.br, body); err != nil {
			return nil, err
		}
		return parseTracks(body)
	}
}

func parseTracks(b []byte) ([]TrackEntry, error) {
	var tracks []TrackEntry
	br := bufio.NewReaderSize(bytes.NewReader(b), 1<<12)

	for {
		trackEntry, err := readHeader(br)
		if err != nil {
			return tracks, catchEOF(err)
		}
		if trackEntry.id != 0xAE {
			if _, err = br.Discard(int(trackEntry.size)); err != nil {
				return nil, catchEOF(err)
			}
			continue
		}
		if err = trackEntry.isBad(defBodyLim); err != nil {
			return nil, err
		}
		var body = make([]byte, trackEntry.size)
		if _, err = io.ReadFull(br, body); err != nil {
			return nil, err
		}

		var track TrackEntry
		err = walk(body, func(id uint32, body []byte) {
			switch id {
			case 0xD7:
				track.TrackNumber = readUint(body)
			case 0x56AA:
				track.CodecDelay = readUint(body)
			case 0x86:
				track.CodecID = string(body)
			case 0x23314F:
				track.TimeScale = readFloat(body)
			case 0x63A2:
				track.CodecPrivate = body
			}
		})
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, track)
	}
}

type TrackEntry struct {
	TrackNumber  uint64
	CodecID      string
	CodecPrivate []byte
	CodecDelay   uint64
	TimeScale    float64
}

func (r *Reader) parseInfo() error {
	for {
		head, err := readHeader(r.br)
		if err != nil {
			return err
		}
		switch head.id {
		case 0x18538067: // Segment (usually has unknown size in live records)
			continue
		case 0x1549A966:
		default:
			if head.unknown {
				return errors.New("unknown size on non-container element")
			}
			if _, err = r.br.Discard(int(head.size)); err != nil {
				return err
			}
			continue
		}
		if err = head.isBad(defBodyLim); err != nil {
			return err
		}
		var body = make([]byte, head.size)
		if _, err = io.ReadFull(r.br, body); err != nil {
			return err
		}
		var timestampScale uint64

		err = walk(body, func(id uint32, body []byte) {
			switch id {
			case 0x2AD7B1:
				timestampScale = readUint(body)
			}
		})
		if err != nil {
			return err
		}
		if timestampScale > 0 {
			r.timeScale = timestampScale
		}
		return nil
	}
}

func readUint(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// readFloat https://www.rfc-editor.org/rfc/rfc8794
// A Float Element MUST declare a length of either zero octets (0 bit), four octets (32 bit), or eight octets (64 bit)
func readFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return 0
}
func catchEOF(err error) error {
	if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}
