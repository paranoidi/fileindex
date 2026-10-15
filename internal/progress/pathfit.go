package progress

import (
	"path/filepath"
	"strings"

	"github.com/mattn/go-runewidth"
)

// Ported from paras-commander internal/primitive/path_fit.go.

const ellipsis = '…'

// fitPath formats path to fit within width terminal cells. The basename is kept
// intact when possible; the longest directories are shortened first (full name →
// first rune + … → single rune). If that is not enough, the whole string is
// clipped in the middle.
func fitPath(path string, width int) string {
	if width <= 0 || path == "" {
		return ""
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "~" {
		path = "~/"
	}
	prefix, segs := parseDisplayPath(path)
	if len(segs) == 0 {
		return truncateMiddle(path, width)
	}
	base, dirs := segs[len(segs)-1], append([]string(nil), segs[:len(segs)-1]...)
	if runewidth.StringWidth(base) > width {
		return truncateMiddle(joinPathParts(prefix, dirs, base), width)
	}
	for {
		candidate := joinPathParts(prefix, dirs, base)
		if runewidth.StringWidth(candidate) <= width {
			return candidate
		}
		best, bestLen := -1, -1
		for i, s := range dirs {
			if _, ok := nextShorterSegmentForm(s); ok {
				if l := runewidth.StringWidth(s); l > bestLen {
					best, bestLen = i, l
				}
			}
		}
		if best < 0 {
			return truncateMiddle(candidate, width)
		}
		dirs[best], _ = nextShorterSegmentForm(dirs[best])
	}
}

func parseDisplayPath(path string) (prefix string, segs []string) {
	for _, p := range []string{"~/", "/"} {
		if strings.HasPrefix(path, p) {
			if rest := path[len(p):]; rest != "" {
				return p, strings.Split(rest, "/")
			}
			return p, nil
		}
	}
	return "", strings.Split(path, "/")
}

func joinPathParts(prefix string, dirs []string, base string) string {
	return prefix + strings.Join(append(append([]string(nil), dirs...), base), "/")
}

func nextShorterSegmentForm(seg string) (string, bool) {
	rr := []rune(seg)
	switch {
	case len(rr) <= 1:
		return "", false
	case len(rr) == 2:
		return string(rr[0]), true
	}
	return string(rr[0]) + string(ellipsis), true
}

// truncateMiddle clips value to width terminal cells, preserving both ends.
func truncateMiddle(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(value) <= width {
		return value
	}
	if width <= 3 {
		return takeCells(value, width)
	}
	prefix := (width - 1) / 2
	return takeCells(value, prefix) + string(ellipsis) + takeCellsRight(value, width-prefix-1)
}

func takeCells(value string, width int) string {
	var b strings.Builder
	used := 0
	for _, r := range value {
		rw := runewidth.RuneWidth(r)
		if rw == 0 {
			if used > 0 {
				b.WriteRune(r)
			}
			continue
		}
		if used+rw > width {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String()
}

func takeCellsRight(value string, width int) string {
	runes := []rune(value)
	used, start := 0, len(runes)
	for i := len(runes) - 1; i >= 0; i-- {
		rw := runewidth.RuneWidth(runes[i])
		if rw > 0 {
			if used+rw > width {
				break
			}
			used += rw
		}
		start = i
	}
	return string(runes[start:])
}
