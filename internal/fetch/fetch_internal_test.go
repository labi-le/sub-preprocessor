package fetch

import (
	"bytes"
	"io"
	"testing"
)

// chunkedReader delivers its payload a few bytes at a time, the way a socket
// does, so a sized read has to loop rather than land in one Read.
type chunkedReader struct {
	src   []byte
	off   int
	chunk int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.off >= len(c.src) {
		return 0, io.EOF
	}
	if len(p) > c.chunk {
		p = p[:c.chunk]
	}
	n := copy(p, c.src[c.off:])
	c.off += n
	return n, nil
}

// TestReadBodyHonoursLimitWhateverTheHeaderClaims pins readBody against the
// ways a Content-Length can disagree with the body behind it. Every case is a
// server the worker actually meets: an honest one, one whose body grew between
// the header and the write, one that overstates, and one that states nothing.
// The announced length must never truncate, never pad and never raise the cap.
func TestReadBodyHonoursLimitWhateverTheHeaderClaims(t *testing.T) {
	t.Parallel()

	const limit = 64
	body := bytes.Repeat([]byte("ab"), 10) // 20 bytes

	cases := []struct {
		name string
		hint int64
		body []byte
		want []byte
	}{
		{"exact", 20, body, body},
		{"announced longer than body", 40, body, body},
		{"body longer than announced", 5, body, body},
		{"no announcement", -1, body, body},
		{"zero announcement with a body", 0, body, body},
		{"announced over the cap", limit + 100, body, body},
		{"body exactly at the cap", limit, bytes.Repeat([]byte("c"), limit), bytes.Repeat([]byte("c"), limit)},
		{
			"body past the cap keeps the extra byte the caller rejects on",
			limit,
			bytes.Repeat([]byte("c"), limit+9),
			bytes.Repeat([]byte("c"), limit+1),
		},
		{
			"body past the cap under a small announcement",
			4,
			bytes.Repeat([]byte("c"), limit+9),
			bytes.Repeat([]byte("c"), limit+1),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := readBody(&chunkedReader{src: tc.body, chunk: 7}, limit, tc.hint)
			if err != nil {
				t.Fatalf("readBody: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("read %d bytes (%q), want %d (%q)", len(got), got, len(tc.want), tc.want)
			}
		})
	}
}

// TestReadBodySizesExactlyOnceWhenAnnounced is the allocation half of the
// contract: an announced length inside the cap must produce ONE buffer, not a
// growth chain. cap() is the observable — a hint that is ignored, or one used
// to size a bytes.Buffer that then grows for its MinRead slack, lands on a
// size-class capacity well past hint+1.
func TestReadBodySizesExactlyOnceWhenAnnounced(t *testing.T) {
	t.Parallel()

	const size = 4096
	src := bytes.Repeat([]byte("x"), size)
	got, err := readBody(&chunkedReader{src: src, chunk: 512}, 10<<20, size)
	if err != nil {
		t.Fatalf("readBody: %v", err)
	}
	if len(got) != size {
		t.Fatalf("len = %d, want %d", len(got), size)
	}
	if cap(got) != size+1 {
		t.Fatalf("cap = %d, want %d: the announced length did not size the read", cap(got), size+1)
	}
}
