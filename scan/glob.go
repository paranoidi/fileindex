package scan

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

var globCache sync.Map // pattern -> *regexp.Regexp

// Fnmatch is case-sensitive shell-style matching: '*', '?', '[seq]', '[!seq]', backslash
// is a literal, an unclosed '[' is a literal, and the whole name must match.
// Go's path.Match differs on all of the last three.
func Fnmatch(name, pattern string) bool {
	re, ok := globCache.Load(pattern)
	if !ok {
		re, _ = globCache.LoadOrStore(pattern, regexp.MustCompile(translate(pattern)))
	}
	return re.(*regexp.Regexp).MatchString(name)
}

// translate ports fnmatch.translate to Go regexp syntax.
func translate(pat string) string {
	p := []rune(pat)
	n := len(p)
	var b strings.Builder
	b.WriteString(`(?s)^`)
	for i := 0; i < n; {
		c := p[i]
		i++
		switch c {
		case '*':
			for i < n && p[i] == '*' {
				i++
			}
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := i
			if j < n && p[j] == '!' {
				j++
			}
			if j < n && p[j] == ']' {
				j++
			}
			for j < n && p[j] != ']' {
				j++
			}
			if j >= n {
				b.WriteString(`\[`)
				continue
			}
			b.WriteString(class(p[i:j]))
			i = j + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`\z`)
	return b.String()
}

// class turns the body of a [...] (without brackets) into a Go character class,
// following fnmatch.translate: split at range dashes into chunks, drop reversed
// ranges, then the last rune of each chunk and the first of the next form a
// range and every other rune (including any other '-') is a literal.
func class(body []rune) string {
	var chunks [][]rune
	if containsRune(body, '-') {
		i, k := 0, 1
		if body[0] == '!' {
			k = 2
		}
		for {
			d := indexFrom(body, '-', k)
			if d < 0 {
				break
			}
			chunks = append(chunks, body[i:d])
			i, k = d+1, d+3
		}
		if last := body[i:]; len(last) > 0 {
			chunks = append(chunks, last)
		} else {
			chunks[len(chunks)-1] = append(chunks[len(chunks)-1], '-')
		}
		for k := len(chunks) - 1; k > 0; k-- {
			prev, cur := chunks[k-1], chunks[k]
			if prev[len(prev)-1] > cur[0] {
				chunks[k-1] = append(append([]rune{}, prev[:len(prev)-1]...), cur[1:]...)
				chunks = append(chunks[:k], chunks[k+1:]...)
			}
		}
	} else {
		chunks = [][]rune{body}
	}

	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total == 0 {
		return `[^\x00-\x{10FFFF}]` // matches nothing
	}
	if total == 1 && chunks[0][0] == '!' {
		return "."
	}
	var b strings.Builder
	b.WriteString("[")
	first := true
	for k, c := range chunks {
		for idx, r := range c {
			if first && r == '!' {
				b.WriteString("^")
				first = false
				continue
			}
			first = false
			if k > 0 && idx == 0 {
				continue // range end, written with its start
			}
			fmt.Fprintf(&b, `\x{%x}`, r)
			if k < len(chunks)-1 && idx == len(c)-1 {
				fmt.Fprintf(&b, `-\x{%x}`, chunks[k+1][0])
			}
		}
	}
	b.WriteString("]")
	return b.String()
}

func containsRune(s []rune, r rune) bool { return indexFrom(s, r, 0) >= 0 }

func indexFrom(s []rune, r rune, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == r {
			return i
		}
	}
	return -1
}
