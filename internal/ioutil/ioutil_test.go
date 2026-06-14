package ioutil_test

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/ioutil"
)

func TestUnsafeString(t *testing.T) {
	t.Parallel()

	t.Run("normal", func(t *testing.T) {
		t.Parallel()
		b := []byte("hello")
		s := ioutil.UnsafeString(b)
		if s != "hello" {
			t.Fatalf("unexpected string: %q", s)
		}
	})

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		s := ioutil.UnsafeString(nil)
		if s != "" {
			t.Fatalf("expected empty, got %q", s)
		}
	})

	t.Run("empty_slice", func(t *testing.T) {
		t.Parallel()
		s := ioutil.UnsafeString([]byte{})
		if s != "" {
			t.Fatalf("expected empty, got %q", s)
		}
	})
}

func TestForEachLineNormalLines(t *testing.T) {
	t.Parallel()
	body := []byte("line1\nline2\nline3\n")
	var got []string
	ioutil.ForEachLine(body, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 3 || got[0] != "line1" || got[1] != "line2" || got[2] != "line3" {
		t.Fatalf("unexpected lines: %v", got)
	}
}

func TestForEachLineSkipsComments(t *testing.T) {
	t.Parallel()
	body := []byte("line1\n# comment\nline2")
	var got []string
	ioutil.ForEachLine(body, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 2 || got[0] != "line1" || got[1] != "line2" {
		t.Fatalf("unexpected lines: %v", got)
	}
}

func TestForEachLineTrimsWhitespace(t *testing.T) {
	t.Parallel()
	body := []byte("  spaced  \n\tindented\n")
	var got []string
	ioutil.ForEachLine(body, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 2 || got[0] != "spaced" || got[1] != "indented" {
		t.Fatalf("unexpected lines: %v", got)
	}
}

func TestForEachLineSkipsEmpty(t *testing.T) {
	t.Parallel()
	body := []byte("a\n\n\nb\n")
	var got []string
	ioutil.ForEachLine(body, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected lines: %v", got)
	}
}

func TestForEachLineEarlyStop(t *testing.T) {
	t.Parallel()
	body := []byte("a\nb\nc\n")
	var got []string
	ioutil.ForEachLine(body, func(line []byte) bool {
		got = append(got, string(line))
		return len(got) < 2
	})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected lines: %v", got)
	}
}

func TestForEachLineNoTrailingNewline(t *testing.T) {
	t.Parallel()
	body := []byte("a\nb")
	var got []string
	ioutil.ForEachLine(body, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected lines: %v", got)
	}
}

func TestForEachLineSingleLine(t *testing.T) {
	t.Parallel()
	body := []byte("single line")
	var got []string
	ioutil.ForEachLine(body, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 1 || got[0] != "single line" {
		t.Fatalf("unexpected lines: %v", got)
	}
}

func TestForEachLineEmptyBody(t *testing.T) {
	t.Parallel()
	var got []string
	ioutil.ForEachLine(nil, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 0 {
		t.Fatalf("expected no lines, got %v", got)
	}
}

func TestForEachLineCommentOnly(t *testing.T) {
	t.Parallel()
	body := []byte("# just a comment")
	var got []string
	ioutil.ForEachLine(body, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 0 {
		t.Fatalf("expected no lines, got %v", got)
	}
}
