package main

// Waveform overview of an audio file: min/max per block of frames, read
// straight from the file Live plays. On Push 3 standalone Live runs on the
// Push, so clip.file_path is a local path. Tethered (Live on a computer) the
// path does not exist here: no waveform, the clip draws plain.
//
// Formats: WAV (PCM 8/16/24/32, float 32/64, also WAVE_FORMAT_EXTENSIBLE) and
// AIFF/AIFC (PCM big-endian, "sowt" little-endian, "fl32"/"fl64"). Others
// (MP3, FLAC, OGG) get no waveform.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"time"
)

const peakBlock = 256 // frames per min/max pair

type peaks struct {
	rate   float64 // frames per second
	frames int64
	lo, hi []int8 // per block, -127..127
}

// span: min and max between two times (seconds) in the file. ok is false
// when the range is outside the file.
func (p *peaks) span(t0, t1 float64) (lo, hi int8, ok bool) {
	if p == nil || len(p.lo) == 0 {
		return 0, 0, false
	}
	if t1 < t0 {
		t0, t1 = t1, t0
	}
	b0 := int(t0 * p.rate / peakBlock)
	b1 := int(t1 * p.rate / peakBlock)
	if b1 < 0 || b0 >= len(p.lo) {
		return 0, 0, false
	}
	if b0 < 0 {
		b0 = 0
	}
	if b1 >= len(p.lo) {
		b1 = len(p.lo) - 1
	}
	lo, hi = 127, -127
	for i := b0; i <= b1; i++ {
		if p.lo[i] < lo {
			lo = p.lo[i]
		}
		if p.hi[i] > hi {
			hi = p.hi[i]
		}
	}
	return lo, hi, true
}

type audioFormat struct {
	channels  int
	bits      int
	float     bool
	bigEndian bool
	rate      float64
	frames    int64 // -1 = read to the end of the data chunk
}

var errFormat = errors.New("unsupported audio format")

const maxAudioBytes = 2 << 30 // 2 GB: skip bigger files

// readPeaks opens a WAV or AIFF file and builds its overview.
func readPeaks(path string) (*peaks, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	var af audioFormat
	var data io.Reader
	switch {
	case string(hdr[0:4]) == "RIFF" && string(hdr[8:12]) == "WAVE":
		af, data, err = wavChunks(r)
	case string(hdr[0:4]) == "FORM" && (string(hdr[8:12]) == "AIFF" || string(hdr[8:12]) == "AIFC"):
		af, data, err = aiffChunks(r, string(hdr[8:12]) == "AIFC")
	default:
		return nil, errFormat
	}
	if err != nil {
		return nil, err
	}
	return decodePeaks(data, af)
}

func wavChunks(r io.Reader) (audioFormat, io.Reader, error) {
	var af audioFormat
	haveFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return af, nil, errFormat
		}
		id := string(ch[0:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch id {
		case "fmt ":
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil || size < 16 {
				return af, nil, errFormat
			}
			tag := binary.LittleEndian.Uint16(b[0:2])
			af.channels = int(binary.LittleEndian.Uint16(b[2:4]))
			af.rate = float64(binary.LittleEndian.Uint32(b[4:8]))
			af.bits = int(binary.LittleEndian.Uint16(b[14:16]))
			if tag == 0xFFFE && size >= 26 { // extensible: real tag at the start of the GUID
				tag = binary.LittleEndian.Uint16(b[24:26])
			}
			switch tag {
			case 1:
			case 3:
				af.float = true
			default:
				return af, nil, errFormat
			}
			if size%2 == 1 {
				_, _ = io.CopyN(io.Discard, r, 1)
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return af, nil, errFormat
			}
			if size == 0 || size == 0xFFFFFFFF {
				af.frames = -1 // streaming writer did not fill the size in
				return af, r, nil
			}
			fb := int64(af.channels * ((af.bits + 7) / 8))
			if fb > 0 {
				af.frames = size / fb
			}
			return af, io.LimitReader(r, size), nil
		default:
			if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
				return af, nil, errFormat
			}
		}
	}
}

func aiffChunks(r io.Reader, aifc bool) (audioFormat, io.Reader, error) {
	af := audioFormat{bigEndian: true}
	haveComm := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return af, nil, errFormat
		}
		id := string(ch[0:4])
		size := int64(binary.BigEndian.Uint32(ch[4:8]))
		switch id {
		case "COMM":
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil || size < 18 {
				return af, nil, errFormat
			}
			af.channels = int(binary.BigEndian.Uint16(b[0:2]))
			af.frames = int64(binary.BigEndian.Uint32(b[2:6]))
			af.bits = int(binary.BigEndian.Uint16(b[6:8]))
			af.rate = extended80(b[8:18])
			if aifc && size >= 22 {
				switch string(b[18:22]) {
				case "NONE", "twos":
				case "sowt":
					af.bigEndian = false
				case "fl32", "FL32":
					af.float, af.bits = true, 32
				case "fl64", "FL64":
					af.float, af.bits = true, 64
				default:
					return af, nil, errFormat
				}
			}
			if size%2 == 1 {
				_, _ = io.CopyN(io.Discard, r, 1)
			}
			haveComm = true
		case "SSND":
			if !haveComm {
				return af, nil, errFormat
			}
			var h [8]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return af, nil, errFormat
			}
			off := int64(binary.BigEndian.Uint32(h[0:4]))
			if _, err := io.CopyN(io.Discard, r, off); err != nil {
				return af, nil, errFormat
			}
			return af, io.LimitReader(r, size-8-off), nil
		default:
			if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
				return af, nil, errFormat
			}
		}
	}
}

// extended80: the 80-bit IEEE float AIFF stores its sample rate in.
func extended80(b []byte) float64 {
	exp := int(binary.BigEndian.Uint16(b[0:2]) & 0x7FFF)
	mant := binary.BigEndian.Uint64(b[2:10])
	if exp == 0 && mant == 0 {
		return 0
	}
	v := float64(mant) * math.Pow(2, float64(exp-16383-63))
	if b[0]&0x80 != 0 {
		v = -v
	}
	return v
}

func decodePeaks(r io.Reader, af audioFormat) (*peaks, error) {
	bps := (af.bits + 7) / 8
	if af.channels < 1 || af.channels > 32 || af.rate <= 0 || bps < 1 || bps > 8 ||
		(af.float && bps != 4 && bps != 8) {
		return nil, errFormat
	}
	fb := af.channels * bps
	if af.frames > 0 && af.frames*int64(fb) > maxAudioBytes {
		return nil, errors.New("audio file too big")
	}
	sample := sampleDecoder(af, bps)
	p := &peaks{rate: af.rate}
	if af.frames > 0 {
		n := af.frames/peakBlock + 1
		p.lo, p.hi = make([]int8, 0, n), make([]int8, 0, n)
	}
	buf := make([]byte, fb*peakBlock*16)
	lo, hi := 1.0, -1.0
	inBlock := 0
	flush := func() {
		p.lo = append(p.lo, toInt8(lo))
		p.hi = append(p.hi, toInt8(hi))
		lo, hi, inBlock = 1.0, -1.0, 0
	}
	for {
		n, err := io.ReadFull(r, buf)
		n -= n % fb
		for off := 0; off < n; off += fb {
			for c := 0; c < af.channels; c++ {
				v := sample(buf[off+c*bps:])
				if v < lo {
					lo = v
				}
				if v > hi {
					hi = v
				}
			}
			p.frames++
			inBlock++
			if inBlock == peakBlock {
				flush()
			}
		}
		if err != nil {
			break
		}
	}
	if inBlock > 0 {
		flush()
	}
	if p.frames == 0 {
		return nil, errFormat
	}
	return p, nil
}

func toInt8(v float64) int8 {
	v = math.Max(-1, math.Min(1, v))
	return int8(math.Round(v * 127))
}

// sampleDecoder returns a function that reads one sample as -1..1.
func sampleDecoder(af audioFormat, bps int) func([]byte) float64 {
	var bo binary.ByteOrder = binary.LittleEndian
	if af.bigEndian {
		bo = binary.BigEndian
	}
	if af.float {
		if bps == 4 {
			return func(b []byte) float64 { return float64(math.Float32frombits(bo.Uint32(b))) }
		}
		return func(b []byte) float64 { return math.Float64frombits(bo.Uint64(b)) }
	}
	switch bps {
	case 1: // 8-bit: WAV is unsigned, AIFF signed
		if af.bigEndian {
			return func(b []byte) float64 { return float64(int8(b[0])) / 128 }
		}
		return func(b []byte) float64 { return (float64(b[0]) - 128) / 128 }
	case 2:
		return func(b []byte) float64 { return float64(int16(bo.Uint16(b))) / 32768 }
	case 3:
		if af.bigEndian {
			return func(b []byte) float64 {
				return float64(int32(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8)>>8) / 8388608
			}
		}
		return func(b []byte) float64 {
			return float64(int32(uint32(b[2])<<24|uint32(b[1])<<16|uint32(b[0])<<8)>>8) / 8388608
		}
	case 4:
		return func(b []byte) float64 { return float64(int32(bo.Uint32(b))) / 2147483648 }
	}
	return func([]byte) float64 { return 0 }
}

// ---- cache: one file at a time, in the background ----

const peakCacheMax = 48

type peakEntry struct {
	p       *peaks
	failed  bool
	loading bool
	used    time.Time
}

type peakCache struct {
	mu      sync.Mutex
	m       map[string]*peakEntry
	queue   []string
	running bool
	changed func()
	read    func(string) (*peaks, error)
}

func newPeakCache(changed func()) *peakCache {
	return &peakCache{m: map[string]*peakEntry{}, changed: changed, read: readPeaks}
}

// get: the overview of a file, or nil while it loads (or when it cannot).
func (pc *peakCache) get(path string) *peaks {
	if pc == nil || path == "" {
		return nil
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	e := pc.m[path]
	if e == nil {
		e = &peakEntry{loading: true}
		pc.m[path] = e
		pc.queue = append(pc.queue, path)
		if !pc.running {
			pc.running = true
			go pc.work()
		}
	}
	e.used = time.Now()
	return e.p
}

func (pc *peakCache) work() {
	for {
		pc.mu.Lock()
		if len(pc.queue) == 0 {
			pc.running = false
			pc.mu.Unlock()
			return
		}
		path := pc.queue[0]
		pc.queue = pc.queue[1:]
		pc.mu.Unlock()

		t0 := time.Now()
		p, err := pc.read(path)
		if err != nil {
			log.Printf("waveform %s: %v", path, err)
		} else if d := time.Since(t0); d > 500*time.Millisecond {
			log.Printf("waveform %s: read in %v", path, d.Round(time.Millisecond))
		}

		pc.mu.Lock()
		if e := pc.m[path]; e != nil {
			e.p, e.failed, e.loading = p, err != nil, false
		}
		pc.evict()
		pc.mu.Unlock()
		if err == nil && pc.changed != nil {
			pc.changed()
		}
	}
}

// evict drops the least recently drawn files over the limit. mu held.
func (pc *peakCache) evict() {
	if len(pc.m) <= peakCacheMax {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	var all []kv
	for k, e := range pc.m {
		if !e.loading {
			all = append(all, kv{k, e.used})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for i := 0; i < len(all) && len(pc.m) > peakCacheMax; i++ {
		delete(pc.m, all[i].k)
	}
}
