package dbt

import (
	"slices"
	"strconv"
	"strings"
)

// rewriteParams replaces @name placeholders with positional $n parameters and
// returns the distinct names in positional order. Repeated names reuse the
// same $n.
//
// Unlike a regexp it is lexically aware: placeholders are never recognized
// inside '...' / E'...' strings, "..." identifiers, -- and /* */ comments
// (which nest, as in Postgres), or $tag$...$tag$ dollar-quoted bodies. The
// operators @>, @?, @@ and friends are left alone, and so is "a@b".
func rewriteParams(s string) (string, []string) {
	var (
		b    strings.Builder
		args []string
	)
	b.Grow(len(s))

	for i := 0; i < len(s); {
		c := s[i]
		var end int
		switch {
		case c == '\'':
			end = skipSingleQuoted(s, i)
		case c == '"':
			end = skipDoubleQuoted(s, i)
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			end = skipLineComment(s, i)
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end = skipBlockComment(s, i)
		case c == '$':
			end = skipDollarQuoted(s, i)
		case c == '@' && isParamStart(s, i):
			j := i + 1
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			name := s[i+1 : j]
			idx := slices.Index(args, name)
			if idx == -1 {
				args = append(args, name)
				idx = len(args) - 1
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(idx + 1))
			i = j
			continue
		default:
			end = i + 1
		}
		b.WriteString(s[i:end])
		i = end
	}
	return b.String(), args
}

func isParamStart(s string, i int) bool {
	if i+1 >= len(s) || !isIdentStart(s[i+1]) {
		return false // @@, @>, @?, trailing @
	}
	if i > 0 && (isIdentByte(s[i-1]) || s[i-1] == '@') {
		return false // a@b, @@name
	}
	return true
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 0x80 || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isIdentByte(c byte) bool {
	return isIdentStart(c) || ('0' <= c && c <= '9')
}

// skipSingleQuoted returns the index just past the string literal starting at
// s[i]. Unterminated literals run to the end; the Postgres parser reports them.
func skipSingleQuoted(s string, i int) int {
	// E'...' strings additionally treat backslash as an escape.
	escapes := i > 0 && (s[i-1] == 'e' || s[i-1] == 'E') && (i == 1 || !isIdentByte(s[i-2]))
	for j := i + 1; j < len(s); j++ {
		switch {
		case escapes && s[j] == '\\':
			j++
		case s[j] == '\'':
			if j+1 < len(s) && s[j+1] == '\'' {
				j++ // '' escape
				continue
			}
			return j + 1
		}
	}
	return len(s)
}

func skipDoubleQuoted(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] == '"' {
			if j+1 < len(s) && s[j+1] == '"' {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}

func skipLineComment(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j != -1 {
		return i + j + 1
	}
	return len(s)
}

func skipBlockComment(s string, i int) int {
	depth := 0
	for j := i; j+1 < len(s); j++ {
		switch {
		case s[j] == '/' && s[j+1] == '*':
			depth++
			j++
		case s[j] == '*' && s[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(s)
}

// skipDollarQuoted handles $$...$$ and $tag$...$tag$. A lone "$" (as in $1,
// or inside an identifier like a$b) consumes just that byte.
func skipDollarQuoted(s string, i int) int {
	if i > 0 && isIdentByte(s[i-1]) {
		return i + 1
	}
	j := i + 1
	if j < len(s) && isIdentStart(s[j]) {
		for j < len(s) && isIdentByte(s[j]) && s[j] != '$' {
			j++
		}
	}
	if j >= len(s) || s[j] != '$' {
		return i + 1 // $1 and friends
	}
	delim := s[i : j+1]
	if k := strings.Index(s[j+1:], delim); k != -1 {
		return j + 1 + k + len(delim)
	}
	return len(s)
}
