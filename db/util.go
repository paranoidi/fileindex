package db

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// PathParts splits a path into parts: empty and "." segments
// dropped, ".." kept, a leading "/" (or exactly "//") is its own part.
func PathParts(p string) []string {
	var parts []string
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		parts = append(parts, "//")
	case strings.HasPrefix(p, "/"):
		parts = append(parts, "/")
	}
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	return parts
}

// JoinParts is the inverse of PathParts.
func JoinParts(parts []string) string {
	if len(parts) == 0 {
		return "."
	}
	root, rest := "", parts
	if parts[0] == "/" || parts[0] == "//" {
		root, rest = parts[0], parts[1:]
	}
	return root + strings.Join(rest, "/")
}

// Lower is full Unicode lowercasing: Go's simple mapping gets İ wrong (it is i + combining dot).
// ponytail: final-sigma (Σ at word end -> ς) not handled; add if Greek names show up.
func Lower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// Resolve makes a path absolute, cleaned, with symlinks
// resolved on the longest prefix that exists and the rest appended as written.
func Resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var tail []string
	for cur := abs; ; cur = filepath.Dir(cur) {
		// ponytail: a dangling symlink stays unresolved instead of following it to its missing target
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, tail...)...), nil
		}
		if cur == "/" {
			return abs, nil
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
	}
}

var units = map[string]float64{
	"B": 1,
	"K": 1 << 10, "KB": 1e3, "KIB": 1 << 10,
	"M": 1 << 20, "MB": 1e6, "MIB": 1 << 20,
	"G": 1 << 30, "GB": 1e9, "GIB": 1 << 30,
	"T": 1 << 40, "TB": 1e12, "TIB": 1 << 40,
}

// ParseSize parses "10K", "2.5MB", "5 GiB": bare K/M/G/T are binary, KB/MB/GB/TB decimal.
func ParseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.TrimSpace(s[i:])
	if num == "" {
		return 0, fmt.Errorf("Invalid size format: %s", s)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("Invalid numeric value in size: %s", s)
	}
	if unit == "" {
		return int64(v), nil
	}
	m, ok := units[unit]
	if !ok {
		return 0, fmt.Errorf("Unknown size unit: %s in %s", unit, s)
	}
	return int64(math.Trunc(v * m)), nil
}

// FormatSize renders bytes as "0 bytes", "123 bytes", "1.50 KiB" ... "2.00 TiB".
func FormatSize(n int64) string {
	switch f := float64(n); {
	case n == 0:
		return "0 bytes"
	case n >= 1<<40:
		return fmt.Sprintf("%.2f TiB", f/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", f/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", f/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KiB", f/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
