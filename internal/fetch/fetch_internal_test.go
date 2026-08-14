package fetch

import (
	"bytes"
	"io"
	"strconv"
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

// TestReadBodyDoesNotTrustAnAnnouncementPastTheCeiling is the amplification
// half: on GET / the announced length comes from a URL the caller chose, so a
// hostile announcement must not buy a buffer before the body behind it does.
// cap() is the observable — the eager buffer stops at the ceiling, and past it
// the doubling lands exactly on an honoured announcement. The first two cases
// spell the ceiling out rather than read maxEagerBody, so the constant's own
// value is under test: it is a measured figure (the first power of two past the
// corpus p90) and the cost of raising it is paid per concurrent request on a
// user-facing endpoint.
func TestReadBodyDoesNotTrustAnAnnouncementPastTheCeiling(t *testing.T) {
	t.Parallel()

	const limit = 10 << 20

	cases := []struct {
		name    string
		hint    int64
		body    int
		wantCap int
	}{
		{"the cap announced with nothing behind it", limit, 0, 256<<10 + 1},
		{"announced past the ceiling, body under it", 8 << 20, 100 << 10, 256<<10 + 1},
		{"announced past the ceiling and honoured", 1 << 20, 1 << 20, (1 << 20) + 1},
		{"announced past the ceiling, body over it", 2 << 20, (1 << 20) + 7, (2 << 20) + 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src := bytes.Repeat([]byte("y"), tc.body)
			got, err := readBody(&chunkedReader{src: src, chunk: 4096}, limit, tc.hint)
			if err != nil {
				t.Fatalf("readBody: %v", err)
			}
			if !bytes.Equal(got, src) {
				t.Fatalf("read %d bytes, want %d", len(got), tc.body)
			}
			if cap(got) != tc.wantCap {
				t.Fatalf("cap = %d, want %d: the announcement was trusted for the wrong amount", cap(got), tc.wantCap)
			}
		})
	}
}

// TestGrowBodyBoundsTheCopyOverlap pins the transient, not the result: a growth
// step copies, so the old buffer and the new one are live at once, and a step
// landing just under the ceiling costs ~2x the cap in simultaneous bytes (20.98
// MB measured before this bound, where the growth chain is what an announcement
// of limit-1 overrun by a byte produces). Each hint walks its whole chain to the
// ceiling, the way an overrunning peer does.
func TestGrowBodyBoundsTheCopyOverlap(t *testing.T) {
	t.Parallel()

	const (
		limit    = int64(10 << 20)
		ceiling  = limit + 1
		half     = (ceiling + 1) / 2
		wantPeak = ceiling + half
	)

	for _, hint := range []int64{limit, limit - 1, 5 << 20, 256 << 10} {
		t.Run(strconv.FormatInt(hint, 10), func(t *testing.T) {
			t.Parallel()

			buf := make([]byte, min(hint, maxEagerBody)+1)
			peak := int64(0)
			for int64(len(buf)) < ceiling {
				next := growBody(buf, hint, limit)
				size, grown := int64(len(buf)), int64(len(next))
				if grown <= size {
					t.Fatalf("grew %d -> %d: no progress", size, grown)
				}
				if grown > size+size {
					t.Fatalf("grew %d -> %d: past twice what has arrived", size, grown)
				}
				if size+grown > peak {
					peak = size + grown
				}
				buf = next
			}
			if peak > wantPeak {
				t.Fatalf("peak %d bytes live at once, want at most %d", peak, wantPeak)
			}
		})
	}
}

// BenchmarkReadBody is where the hint's remaining value is checkable: the three
// sizes are the configured corpus's median, p90 and largest source body, and
// each runs with the length announced and withheld. B/op is the point, ns/op is
// noise — an announced body at or under the ceiling allocates once, and one
// above it pays a bounded doubling chain instead.
func BenchmarkReadBody(b *testing.B) {
	const limit = 10 << 20

	sizes := []struct {
		name string
		size int
	}{
		{"median_41704", 41704},
		{"p90_198272", 198272},
		{"largest_3058844", 3058844},
	}

	for _, s := range sizes {
		src := bytes.Repeat([]byte("z"), s.size)
		for _, announced := range []bool{true, false} {
			hint := int64(0)
			label := "unannounced"
			if announced {
				hint = int64(s.size)
				label = "announced"
			}
			b.Run(s.name+"/"+label, func(b *testing.B) {
				b.SetBytes(int64(s.size))
				b.ReportAllocs()
				for range b.N {
					got, err := readBody(&chunkedReader{src: src, chunk: 32 << 10}, limit, hint)
					if err != nil || len(got) != s.size {
						b.Fatalf("readBody: %d bytes, %v", len(got), err)
					}
				}
			})
		}
	}
}
