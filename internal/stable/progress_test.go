package stable //nolint:testpackage // exercises unexported stable internals

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestProgressMilestones(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	p := newProgress(logger, "probe progress", 20)
	for range 20 {
		p.step()
	}
	// One info line per 10% decade crossing: 2,4,...,20.
	if got := strings.Count(buf.String(), "probe progress"); got != 10 {
		t.Fatalf("total=20 -> 10 milestones, got %d\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), `"done":20,"total":20`) {
		t.Fatalf("final milestone must report done=total:\n%s", buf.String())
	}

	buf.Reset()
	small := newProgress(logger, "probe progress", 3)
	for range 3 {
		small.step()
	}
	if got := strings.Count(buf.String(), "probe progress"); got != 3 {
		t.Fatalf("total=3 -> every step is a decade crossing, got %d", got)
	}

	buf.Reset()
	empty := newProgress(logger, "probe progress", 0)
	if n := empty.step(); n != 1 {
		t.Fatalf("step ordinal = %d, want 1", n)
	}
	if buf.Len() != 0 {
		t.Fatalf("total=0 must never log, got %q", buf.String())
	}
}
