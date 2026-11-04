package progress

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"
	"golang.org/x/sys/unix"
)

// Progress redraws a block of status lines (current dir, counts, hashing speed,
// one line per worker) with ANSI cursor moves. It is a no-op unless out is a terminal.
// Safe for concurrent use.
type Progress struct {
	mu                         sync.Mutex
	out                        io.Writer
	fd                         int
	active                     bool
	roots                      []string
	workers                    []string // per worker id-1; "" = idle
	dir                        string
	scanned, newFiles, skipped int
	bps                        float64 // < 0 = not measured yet
	widths                     []int   // display width of each drawn line
	pruning                    bool
	hashing                    bool // hashing stage: a total and a done count instead of the scan lines
	hashTotal, hashDone        int
	winch                      chan os.Signal
	resizing                   bool // window resize in progress: hold redraws until it settles
}

// resizeSettle is how long the window size must stay put before redrawing.
const resizeSettle = 150 * time.Millisecond

func NewProgress(out io.Writer, workers int, roots []string, quiet bool) *Progress {
	p := &Progress{out: out, roots: roots, workers: make([]string, max(workers, 0)), bps: -1}
	if f, ok := out.(*os.File); ok && !quiet {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			p.active, p.fd = true, int(f.Fd())
			p.winch = make(chan os.Signal, 1)
			signal.Notify(p.winch, unix.SIGWINCH)
			go p.watchResize()
		}
	}
	return p
}

// watchResize holds redraws while the window is being resized. Mid-drag redraws
// count rows at a width the terminal has already left, leaving ghost lines; once
// the size settles, the reflowed block is counted at its final width.
func (p *Progress) watchResize() {
	settle := time.NewTimer(resizeSettle)
	settle.Stop()
	for {
		select {
		case _, ok := <-p.winch:
			if !ok {
				settle.Stop()
				return
			}
			p.mu.Lock()
			p.resizing = true
			p.mu.Unlock()
			settle.Reset(resizeSettle)
		case <-settle.C:
			p.mu.Lock()
			p.resizing = false
			p.redraw()
			p.mu.Unlock()
		}
	}
}

func (p *Progress) Worker(id int, path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id >= 1 && id <= len(p.workers) {
		p.workers[id-1] = path
	}
	p.redraw()
}

// FileDone counts one hashed file, failed ones too.
func (p *Progress) FileDone() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hashDone++
	p.redraw()
}

// Speed updates the hashing speed and the number of worker lines.
func (p *Progress) Speed(workers int, bps float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bps = bps
	if workers < len(p.workers) {
		p.workers = p.workers[:workers]
	}
	for len(p.workers) < workers {
		p.workers = append(p.workers, "")
	}
	p.redraw()
}

// Scan updates the discovery line; dir "" means idle.
func (p *Progress) Scan(dir string, scanned, newFiles, skipped int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dir, p.scanned, p.newFiles, p.skipped = dir, scanned, newFiles, skipped
	p.redraw()
}

// Prune switches the block to pruning: the dir being checked and files checked so far.
func (p *Progress) Prune(dir string, checked int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruning, p.dir, p.scanned = true, dir, checked
	p.redraw()
}

// Hashing switches the block to hashing total files: files done so far and
// one line per worker.
func (p *Progress) Hashing(total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hashing, p.hashTotal, p.hashDone = true, total, 0
	p.redraw()
}

// rel shows a path relative to the first base containing it.
func rel(path string, bases ...string) string {
	out := path
	for _, r := range bases {
		if r == "" {
			continue
		}
		if rp, err := filepath.Rel(r, path); err == nil && rp != ".." && !strings.HasPrefix(rp, "../") {
			out = rp
			break
		}
	}
	return out
}

// labeled fits path into the cols-1 cells left after label.
func labeled(label, path string, cols int) string {
	return label + fitPath(path, cols-1-runewidth.StringWidth(label))
}

func (p *Progress) cols() int {
	if ws, err := unix.IoctlGetWinsize(p.fd, unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
		return int(ws.Col)
	}
	return 80
}

// up moves the cursor to the top of the drawn block. Rows are counted at the
// current width: a terminal that reflows re-wraps old lines after a shrink.
func (p *Progress) up(cols int) string {
	rows := 0
	for _, w := range p.widths {
		rows += max(1, (w+cols-1)/cols)
	}
	p.widths = nil
	if rows == 0 {
		return ""
	}
	return fmt.Sprintf("\x1b[%dA", rows)
}

func (p *Progress) redraw() {
	if !p.active || p.resizing {
		return
	}
	cols := p.cols()
	lines := []string{"Scanning: [idle]", fmt.Sprintf("Scanned: %d", p.scanned)}
	if p.dir != "" {
		lines[0] = labeled("Scanning: ", rel(p.dir, p.roots...), cols)
	}
	if p.pruning {
		lines = []string{labeled("Pruning: ", rel(p.dir, p.roots...), cols), fmt.Sprintf("Checked: %d", p.scanned)}
	}
	if p.hashing {
		lines = []string{fmt.Sprintf("Hashed: %d of %d files", p.hashDone, p.hashTotal)}
	}
	if p.newFiles > 0 && !p.hashing {
		lines[1] += fmt.Sprintf(", New: %d", p.newFiles)
	}
	if p.skipped > 0 && !p.hashing {
		lines[1] += fmt.Sprintf(", Skipped: %d", p.skipped)
	}
	if p.bps >= 0 {
		lines = append(lines, fmt.Sprintf("Speed: %.1f MB/s, Workers: %d", p.bps/1048576, len(p.workers)))
	}
	for i, w := range p.workers {
		label := fmt.Sprintf("Worker %d: ", i+1)
		if w == "" {
			lines = append(lines, label+"[idle]")
			continue
		}
		lines = append(lines, labeled(label, rel(w, p.roots...), cols))
	}
	var b strings.Builder
	b.WriteString(p.up(cols))
	for _, l := range lines {
		l = runewidth.Truncate(l, cols-1, "")
		b.WriteString("\r\x1b[K" + l + "\n")
		p.widths = append(p.widths, runewidth.StringWidth(l))
	}
	b.WriteString("\x1b[J") // drop lines left over from a taller block
	io.WriteString(p.out, b.String())
}

// Print writes b to w above the block: erase it, write, redraw. Keeps log lines
// (on another fd of the same terminal) from desyncing the cursor-relative redraw.
func (p *Progress) Print(w io.Writer, b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		_, _ = w.Write(b)
		return
	}
	if up := p.up(p.cols()); up != "" {
		io.WriteString(p.out, up+"\r\x1b[J")
	}
	_, _ = w.Write(b)
	p.redraw()
}

// Clear blanks the block, leaves the cursor at its top and stops further drawing.
func (p *Progress) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		return
	}
	p.active = false
	signal.Stop(p.winch)
	close(p.winch)
	if up := p.up(p.cols()); up != "" {
		io.WriteString(p.out, up+"\r\x1b[J")
	}
}
