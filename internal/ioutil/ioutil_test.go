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
