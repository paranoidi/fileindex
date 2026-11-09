package scan

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hashes are lowercase hex. NoExifJPEG is set only for .jpg/.jpeg files that strip cleanly.
// Sample is the sample hash (sampleRanges).
type Hashes struct {
	MD5, SHA256, Sample string
	NoExifJPEG          *string
}

// Result is one hashed file. Size and Mtime come from the stat taken before
// hashing; Err is a ready-to-print message.
type Result struct {
	Path   string
	Hashes Hashes
	Size   int64
	Mtime  time.Time
	Err    error
}

// afterHash lets tests change a file between hashing and the second stat.
var afterHash = func(string) {}

// HashFile hashes one file. It stats before and after hashing and reports the
// file as changed if size or mtime moved, so stored size always matches the
// hashes.
func HashFile(path string) Result { return hashFile(path, new(atomic.Int64), false) }

// sampleSize is the length of each piece of a file the sample hash covers.
const sampleSize = 8192

// sampleRanges are the [start, end) byte ranges the sample hash covers in a file
// of size: its first, middle and last sampleSize bytes, or the whole file when
// those would touch. The tail catches a file cut short or still being written
// into a preallocated full size; the middle, a gap left in between.
func sampleRanges(size int64) [][2]int64 {
	if size <= 3*sampleSize {
		return [][2]int64{{0, size}}
	}
	mid := size/2 - sampleSize/2
	return [][2]int64{{0, sampleSize}, {mid, mid + sampleSize}, {size - sampleSize, size}}
}

// sampleRead is how many bytes a sample-only hash reads from a file of size.
func sampleRead(size int64) int64 { return min(size, 3*sampleSize) }

// hashFile is HashFile that adds bytes to read as they are read. With fast, a
// file larger than its sample gets only Hashes.Sample; a smaller one is read
// whole anyway, so it gets every hash.
func hashFile(path string, read *atomic.Int64, fast bool) Result {
	r := Result{Path: path}
	before, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		r.Err = fmt.Errorf("File not found: %s", path)
		return r
	case err != nil:
		r.Err = fmt.Errorf("Error reading file %s: %v", path, err)
		return r
	case !before.Mode().IsRegular():
		r.Err = fmt.Errorf("Path is not a regular file: %s", path)
		return r
	}
	r.Size, r.Mtime = before.Size(), before.ModTime()

	hash := hashContent
	if fast && sampleRead(before.Size()) < before.Size() {
		hash = hashSample
	}
	if r.Hashes, err = hash(path, before.Size(), read); err != nil {
		if pe, ok := err.(*os.PathError); ok {
			err = pe.Err // the path is already in the message
		}
		r.Err = fmt.Errorf("Error reading file %s: %v", path, err)
		return r
	}
	afterHash(path)
	after, err := os.Stat(path)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		r.Err = fmt.Errorf("File changed during hashing: %s", path)
	}
	return r
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// sampler is a writer that hashes the sampleRanges bytes of the stream written
// to it, which must start at offset 0.
type sampler struct {
	h      hash.Hash
	ranges [][2]int64
	off    int64
}

func newSampler(size int64) *sampler { return &sampler{h: sha256.New(), ranges: sampleRanges(size)} }

func (s *sampler) Write(p []byte) (int, error) {
	start, end := s.off, s.off+int64(len(p))
	for _, r := range s.ranges {
		if lo, hi := max(r[0], start), min(r[1], end); lo < hi {
			_, _ = s.h.Write(p[lo-start : hi-start]) // hash writers never fail
		}
	}
	s.off = end
	return len(p), nil
}

func (s *sampler) sum() string { return hex.EncodeToString(s.h.Sum(nil)) }

// hashSample reads only the sampleRanges of a file of size.
func hashSample(path string, size int64, read *atomic.Int64) (Hashes, error) {
	file, err := os.Open(path)
	if err != nil {
		return Hashes{}, err
	}
	defer file.Close()
	s := newSampler(size)
	for _, r := range s.ranges {
		buf := make([]byte, r[1]-r[0])
		if _, err := io.ReadFull(countingReader{io.NewSectionReader(file, r[0], r[1]-r[0]), read}, buf); err != nil {
			return Hashes{}, err
		}
		s.off = r[0]
		_, _ = s.Write(buf)
	}
	return Hashes{Sample: s.sum()}, nil
}

// hashContent reads the whole file, size bytes at the stat before hashing, for
// every hash.
func hashContent(path string, size int64, read *atomic.Int64) (Hashes, error) {
	var h Hashes
	file, err := os.Open(path)
	if err != nil {
		return h, err
	}
	defer file.Close()
	f := countingReader{file, read}
	smp := newSampler(size)

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".jpg" || ext == ".jpeg" {
		data, err := io.ReadAll(f)
		if err != nil {
			return h, err
		}
		_, _ = smp.Write(data)
		h.MD5, h.SHA256, h.Sample = md5Hex(data), sha256Hex(data), smp.sum()
		if stripped, err := StripJPEGMetadata(data); err == nil {
			s := sha256Hex(stripped)
			h.NoExifJPEG = &s
		} else {
			Logger.Warn(fmt.Sprintf("Error calculating metadata-stripped hash for %s: %v (type: ValueError)", path, err))
		}
		return h, nil
	}

	m, s := md5.New(), sha256.New()
	if _, err := io.Copy(io.MultiWriter(m, s, smp), f); err != nil {
		return h, err
	}
	h.MD5, h.SHA256, h.Sample = hex.EncodeToString(m.Sum(nil)), hex.EncodeToString(s.Sum(nil)), smp.sum()
	return h, nil
}

func md5Hex(b []byte) string    { s := md5.Sum(b); return hex.EncodeToString(s[:]) }
func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// tick is how often HashAll measures speed and may rescale; holdTicks is how
// many steady ticks pass before auto mode probes another worker count.
var tick = 2 * time.Second

const holdTicks = 5

// HashAll hashes paths with `workers` goroutines and yields results in
// completion order (input order when workers == 1). workers == 0 auto-scales:
// start at 1 and hill-climb on bytes hashed per second (see scaler).
// onStart, if set, is called from worker goroutines as (worker id 1..N, path);
// onTick, if set, is called every tick as (workers, bytes/s). The caller
// serialises any output. Stopping the iteration or cancelling ctx stops the
// walk and workers. fast is as in hashFile.
func HashAll(ctx context.Context, paths iter.Seq[string], workers int, fast bool, onStart func(int, string), onTick func(int, float64)) iter.Seq[Result] {
	return func(yield func(Result) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		in := make(chan string)
		out := make(chan Result)
		fed := make(chan struct{})
		go func() {
			defer close(fed)
			defer close(in)
			for p := range paths {
				select {
				case in <- p:
				case <-ctx.Done():
					return
				}
			}
		}()

		var read atomic.Int64
		var wg sync.WaitGroup
		var quits []chan struct{} // per running worker id-1; closing one retires it
		spawn := func() {
			id, quit := len(quits)+1, make(chan struct{})
			quits = append(quits, quit)
			wg.Go(func() {
				for {
					var p string
					select {
					case q, ok := <-in:
						if !ok {
							return
						}
						p = q
					case <-quit:
						return
					}
					if onStart != nil {
						onStart(id, p)
					}
					select {
					case out <- hashFile(p, &read, fast):
					case <-ctx.Done():
						return
					}
				}
			})
		}
		resize := func(n int) {
			for len(quits) < n {
				spawn()
			}
			for len(quits) > n { // the retired worker finishes its current file first
				close(quits[len(quits)-1])
				quits = quits[:len(quits)-1]
			}
		}

		// This goroutine holds a wg slot while it may still spawn, so close(out)
		// can't race a spawn; it lets go once every path has been handed out.
		wg.Add(1)
		spawning := true
		release := func() {
			if spawning {
				spawning = false
				wg.Done()
			}
		}
		defer release()
		go func() { wg.Wait(); close(out) }()

		s := scaler{n: max(workers, 1), hold: holdTicks - 1}
		resize(s.n)
		t := time.NewTicker(tick)
		defer t.Stop()
		last := time.Now()
		for {
			select {
			case r, ok := <-out:
				if !ok {
					return
				}
				if !yield(r) {
					return
				}
			case <-fed:
				fed = nil
				release()
			case now := <-t.C:
				bps := float64(read.Swap(0)) / now.Sub(last).Seconds()
				last = now
				if workers == 0 && spawning {
					resize(s.step(bps))
				}
				if onTick != nil {
					onTick(len(quits), bps)
				}
			}
		}
	}
}

// scaler hill-climbs the worker count on hashing speed. It probes one step
// (alternately up and down), keeps going while that pays off and undoes it
// when it doesn't. Going up must gain 5%; going down may lose up to 5%, so
// ties favour fewer workers.
// ponytail: no upper cap; the 5% margin stops growth. Add one if noisy disks overshoot.
type scaler struct {
	n    int
	dir  int     // move being tried: +1, -1, or 0 when holding
	hold int     // steady ticks since the last decision
	prev float64 // speed before the move being tried
	down bool    // next probe goes down
}

func (s *scaler) step(bps float64) int {
	if bps == 0 { // nothing read (e.g. waiting on discovery): no signal
		return s.n
	}
	switch {
	case s.dir > 0 && bps > s.prev*1.05, s.dir < 0 && bps >= s.prev*0.95:
		// paid off: keep going
	case s.dir != 0:
		s.n -= s.dir // undo, keep the old baseline
		s.dir, s.hold = 0, 0
		return s.n
	default:
		s.prev = bps
		if s.hold++; s.hold < holdTicks {
			return s.n
		}
		s.hold, s.dir = 0, 1
		if s.down && s.n > 1 {
			s.dir = -1
		}
		s.down = !s.down
	}
	if s.n+s.dir < 1 {
		s.dir = 0
		return s.n
	}
	s.prev = bps
	s.n += s.dir
	return s.n
}
