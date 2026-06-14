// Package ioutil provides shared I/O utilities used across the application.
package ioutil

import (
	"bytes"
	"unsafe"
)

// UnsafeString converts a byte slice to a string without copying.
// The resulting string references the same memory as b. The caller must
// ensure b is not modified while the string is in use.
func UnsafeString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// Lines iterates over non-empty, non-comment lines in a byte slice.
// Each returned line is trimmed of leading and trailing whitespace.
// Lines starting with '#' (after trim) are skipped.
//
// Usage:
//
//	it := ioutil.NewLines(body)
//	for {
//	    line := it.Next()
//	    if line == nil {
//	        break
//	    }
//	    // process line
//	}
type Lines struct {
	remain []byte
}

// NewLines creates a Lines iterator over body.
func NewLines(body []byte) Lines {
	return Lines{remain: body}
}

// Next returns the next trimmed line, or nil when exhausted.
func (l *Lines) Next() []byte {
	for {
		if len(l.remain) == 0 {
			return nil
		}

		idx := bytes.IndexByte(l.remain, '\n')
		if idx < 0 {
			line := bytes.TrimSpace(l.remain)
			l.remain = l.remain[:0]
			if len(line) > 0 && line[0] != '#' {
				return line
			}
			return nil
		}

		line := bytes.TrimSpace(l.remain[:idx])
		l.remain = l.remain[idx+1:]
		if len(line) > 0 && line[0] != '#' {
			return line
		}
	}
}

// ForEachLine iterates over lines in body. Each line is trimmed of leading
// and trailing whitespace. Empty lines and lines starting with '#' are
// skipped. If fn returns false, iteration stops early.
func ForEachLine(body []byte, fn func(line []byte) bool) {
	it := NewLines(body)
	for {
		line := it.Next()
		if line == nil {
			return
		}
		if !fn(line) {
			return
		}
	}
}
