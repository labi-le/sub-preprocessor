package stable_test

import (
	"bytes"
	"testing"

	"domains.lst/sub-preprocessor/internal/stable"
)

func entry(label string) stable.Entry {
	return stable.Entry{Label: label, Raw: "vless://u@" + label + ".example:443#" + label}
}

func TestSelectSurvivorsFiltersAndSorts(t *testing.T) {
	t.Parallel()

	entries := []stable.Entry{
		entry("a"), // all rounds ok, slow-ish
		entry("b"), // one failure
		entry("c"), // missing from results entirely
		entry("d"), // mean exactly at limit
		entry("e"), // mean above limit
	}
	res := map[string]stable.ProbeResult{
		"a": {Successes: 5, MeanMs: 200},
		"b": {Successes: 4, MeanMs: 100},
		"d": {Successes: 5, MeanMs: 1000},
		"e": {Successes: 5, MeanMs: 1001},
	}

	got := stable.SelectSurvivors(entries, res, 5, 0, 1000)
	wantLabels := []string{"a", "d"}
	if len(got) != len(wantLabels) {
		t.Fatalf("got %d survivors %+v, want %v", len(got), got, wantLabels)
	}
	for i, w := range wantLabels {
		if got[i].Label != w {
			t.Errorf("survivor %d: got %q, want %q", i, got[i].Label, w)
		}
	}

	// maxFail=1 admits b, which sorts first by mean.
	got = stable.SelectSurvivors(entries, res, 5, 1, 1000)
	wantLabels = []string{"b", "a", "d"}
	if len(got) != len(wantLabels) {
		t.Fatalf("got %d survivors %+v, want %v", len(got), got, wantLabels)
	}
	for i, w := range wantLabels {
		if got[i].Label != w {
			t.Errorf("survivor %d: got %q, want %q", i, got[i].Label, w)
		}
	}
}

func TestBuildPayload(t *testing.T) {
	t.Parallel()

	survivors := []stable.Survivor{
		{Entry: stable.Entry{Label: "x", Raw: "vless://u@x:443#x"}, MeanMs: 10},
		{Entry: stable.Entry{Label: "y", Raw: "vless://u@y:443#y"}, MeanMs: 20},
	}
	want := []byte("vless://u@x:443#x\nvless://u@y:443#y\n")
	if got := stable.BuildPayload(survivors); !bytes.Equal(got, want) {
		t.Fatalf("payload mismatch:\ngot  %q\nwant %q", got, want)
	}
	if got := stable.BuildPayload(nil); len(got) != 0 {
		t.Fatalf("empty survivors should give empty payload, got %q", got)
	}
}
