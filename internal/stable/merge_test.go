package stable_test

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/stable"
)

func TestMergeDedupesAndRelabels(t *testing.T) {
	t.Parallel()

	alpha := []byte("vless://uuid-a@host.example:443?type=tcp#Old Name\n" +
		"garbage line without scheme\n" +
		"vless://uuid-b@other.example:8443\n")
	beta := []byte("vless://uuid-c@host.example:443#Dup Of Alpha\n" +
		"vless://uuid-d@beta.example:443#Beta Node\n")

	entries := stable.Merge([]stable.SourceBody{
		{Name: "alpha", Body: alpha},
		{Name: "beta", Body: beta},
	})

	want := []stable.Entry{
		{Label: "alpha-001", Raw: "vless://uuid-a@host.example:443?type=tcp#alpha-001"},
		{Label: "alpha-002", Raw: "vless://uuid-b@other.example:8443#alpha-002"},
		{Label: "beta-001", Raw: "vless://uuid-d@beta.example:443#beta-001"},
	}

	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("entry %d: got %+v, want %+v", i, entries[i], w)
		}
	}
}

func TestMergeEmptyInput(t *testing.T) {
	t.Parallel()

	if got := stable.Merge(nil); len(got) != 0 {
		t.Fatalf("expected no entries, got %+v", got)
	}
}
