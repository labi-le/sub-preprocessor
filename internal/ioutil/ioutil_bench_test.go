package ioutil_test

import (
	"bytes"
	"testing"

	"domains.lst/sub-preprocessor/internal/ioutil"
)

var sinkInt int

func buildLinesBody() []byte {
	var buf bytes.Buffer
	// Build a multi-KB body mixing data lines, comment lines and blanks.
	for i := 0; buf.Len() < 8<<10; i++ {
		switch i % 4 {
		case 0:
			buf.WriteString("# this is a comment line that should be skipped\n")
		case 1:
			buf.WriteString("\n")
		case 2:
			buf.WriteString("  vless://uuid@host.example:443?type=tcp#Node Name  \n")
		default:
			buf.WriteString("ss://YWVzLTI1Ni1nY206cGFzcw@1.2.3.4:8388#Another\n")
		}
	}
	return buf.Bytes()
}

func BenchmarkLinesNext(b *testing.B) {
	body := buildLinesBody()

	b.ReportAllocs()
	for b.Loop() {
		it := ioutil.NewLines(body)
		count := 0
		for {
			line := it.Next()
			if line == nil {
				break
			}
			count++
		}
		sinkInt = count
	}
}

func BenchmarkUnsafeString(b *testing.B) {
	buf := make([]byte, 64)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}

	b.ReportAllocs()
	for b.Loop() {
		s := ioutil.UnsafeString(buf)
		sinkInt = len(s)
	}
}
