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

	t.Run("zero_copy_aliases_the_backing_array", func(t *testing.T) {
		t.Parallel()
		b := []byte("hello")
		s := ioutil.UnsafeString(b)
		b[0] = 'H'
		if s != "Hello" {
			t.Fatalf("UnsafeString copied: after b[0] = 'H', s = %q, want %q", s, "Hello")
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
