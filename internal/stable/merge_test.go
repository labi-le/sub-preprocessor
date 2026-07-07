package stable_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
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

func TestMergeRelabelsVmessViaPs(t *testing.T) {
	t.Parallel()

	body := []byte("vmess://" +
		base64.StdEncoding.EncodeToString([]byte(`{"add":"1.2.3.4","port":"443","ps":"Original","id":"uuid"}`)) + "\n")

	entries := stable.Merge([]stable.SourceBody{{Name: "avia", Body: body}})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Label != "avia-001" {
		t.Errorf("label: got %q, want avia-001", entries[0].Label)
	}

	const prefix = "vmess://"
	raw := entries[0].Raw
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("expected vmess:// entry, got %q", raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw[len(prefix):])
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	var m map[string]any
	if err = json.Unmarshal(decoded, &m); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if m["ps"] != "avia-001" {
		t.Errorf("ps: got %v, want avia-001", m["ps"])
	}
	if m["add"] != "1.2.3.4" {
		t.Errorf("add lost: got %v", m["add"])
	}
}
