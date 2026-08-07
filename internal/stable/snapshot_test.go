package stable_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/stable"
)

func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stable.json")
	want := &stable.Snapshot{
		Payload:   []byte("vless://u@1.1.1.1:443#alpha-001\nvless://u@2.2.2.2:443#beta-001\n"),
		UpdatedAt: time.Date(2026, 8, 7, 13, 53, 57, 0, time.UTC),
		Stats:     stable.Stats{SourcesOK: 2, SourcesTotal: 3, Merged: 68266, Tested: 400, Kept: 129},
	}
	if err := stable.SaveSnapshot(path, want); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	got := stable.LoadSnapshot(path, zerolog.Nop())
	if got == nil {
		t.Fatal("the saved snapshot did not come back; /stable.txt would still answer 503 after a restart")
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Errorf("payload:\ngot  %q\nwant %q", got.Payload, want.Payload)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("updated_at: got %v want %v", got.UpdatedAt, want.UpdatedAt)
	}
	if got.Stats != want.Stats {
		t.Errorf("stats: got %+v want %+v", got.Stats, want.Stats)
	}
}

// TestSnapshotFileShape pins the document an operator reads and an older
// process wrote: one JSON object, the list as readable text (not base64), and
// the five stats under snake_case keys.
func TestSnapshotFileShape(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stable.json")
	if err := stable.SaveSnapshot(path, &stable.Snapshot{
		Payload:   []byte("vless://u@1.1.1.1:443#alpha-001\n"),
		UpdatedAt: time.Date(2026, 8, 7, 13, 53, 57, 0, time.UTC),
		Stats:     stable.Stats{SourcesOK: 1, SourcesTotal: 2, Merged: 3, Tested: 4, Kept: 5},
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Payload   string `json:"payload"`
		UpdatedAt string `json:"updated_at"`
		Stats     struct {
			SourcesOK    int `json:"sources_ok"`
			SourcesTotal int `json:"sources_total"`
			Merged       int `json:"merged"`
			Tested       int `json:"tested"`
			Kept         int `json:"kept"`
		} `json:"stats"`
	}
	if err = json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("snapshot is not the documented JSON document: %v\n%s", err, b)
	}
	if doc.Payload != "vless://u@1.1.1.1:443#alpha-001\n" {
		t.Errorf("payload is not the list as text: %q", doc.Payload)
	}
	if doc.UpdatedAt != "2026-08-07T13:53:57Z" {
		t.Errorf("updated_at is not RFC 3339: %q", doc.UpdatedAt)
	}
	if doc.Stats.SourcesOK != 1 || doc.Stats.SourcesTotal != 2 ||
		doc.Stats.Merged != 3 || doc.Stats.Tested != 4 || doc.Stats.Kept != 5 {
		t.Errorf("stats keys do not carry the five fields: %+v", doc.Stats)
	}
}

func TestLoadSnapshotRejectedInputs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		content string // written to the file; "" means no file at all
		wantLog string
	}{
		{name: "missing", wantLog: "no stable snapshot restored"},
		{name: "truncated mid-write", content: `{"payload":"vless://u@1.1.1`, wantLog: "malformed"},
		{name: "not json at all", content: "vless://u@1.1.1.1:443#alpha-001\n", wantLog: "malformed"},
		{name: "empty list", content: `{"payload":"","updated_at":"2026-08-07T13:53:57Z"}`, wantLog: "empty list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "stable.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var logBuf bytes.Buffer

			got := stable.LoadSnapshot(path, zerolog.New(&logBuf))

			if got != nil {
				t.Error("a rejected snapshot must restore nothing; app.restoreStableList then leaves the holder empty")
			}
			logs := logBuf.String()
			if !strings.Contains(logs, `"level":"warn"`) {
				t.Errorf("a rejected snapshot must warn, got:\n%s", logs)
			}
			if !strings.Contains(logs, tc.wantLog) {
				t.Errorf("warning does not say %q, got:\n%s", tc.wantLog, logs)
			}
		})
	}
}

// TestSnapshotDisabled: the empty path is the documented off switch, and it
// must be silent -- an operator who did not ask for persistence gets no file
// and no warning about one.
func TestSnapshotDisabled(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	if err := stable.SaveSnapshot("", &stable.Snapshot{Payload: []byte("x")}); err != nil {
		t.Fatalf("SaveSnapshot with persistence off: %v", err)
	}
	if stable.LoadSnapshot("", zerolog.New(&logBuf)) != nil {
		t.Error("an empty path must restore nothing")
	}
	if logBuf.Len() != 0 {
		t.Errorf("persistence off must log nothing, got:\n%s", logBuf.String())
	}
}

// TestSaveSnapshotReplacesAtomically proves the publish is a rename and not an
// in-place rewrite: a reader holding the file open across a save keeps reading
// the COMPLETE previous document, which is exactly what "no partial file is
// ever observable" means. A save that truncated and rewrote path would hand
// that reader the new bytes, or half of them.
func TestSaveSnapshotReplacesAtomically(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "stable.json")
	first := &stable.Snapshot{Payload: []byte("first\n"), Stats: stable.Stats{Kept: 1}}
	if err := stable.SaveSnapshot(path, first); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	// A payload far larger than one write can be assumed atomic for, so an
	// in-place rewrite would be observable as a partial document.
	second := &stable.Snapshot{
		Payload: bytes.Repeat([]byte("vless://u@1.1.1.1:443#alpha-001\n"), 20000),
		Stats:   stable.Stats{Kept: 20000},
	}
	if err = stable.SaveSnapshot(path, second); err != nil {
		t.Fatalf("SaveSnapshot over an open reader: %v", err)
	}

	held, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(held, before) {
		t.Errorf("a reader open across the save saw %d bytes of changed content; the replace is not a rename", len(held))
	}
	if got := stable.LoadSnapshot(path, zerolog.Nop()); got == nil || !bytes.Equal(got.Payload, second.Payload) {
		t.Error("the new snapshot is not what the path resolves to after the save")
	}
	assertOnlyFile(t, dir, "stable.json")
}

// TestSaveSnapshotFailureKeepsPreviousFile: a write that cannot start must
// leave the last good snapshot readable and drop no temp file. The read-only
// directory is what makes it deterministic -- creating the sibling temp fails
// while the existing file itself stays writable, so an implementation that
// wrote path directly would report success and destroy the previous list.
func TestSaveSnapshotFailureKeepsPreviousFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "stable.json")
	good := &stable.Snapshot{Payload: []byte("good\n"), Stats: stable.Stats{Kept: 7}}
	if err := stable.SaveSnapshot(path, good); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := stable.SaveSnapshot(path, &stable.Snapshot{Payload: []byte("newer\n")})

	if err == nil {
		t.Fatal("a snapshot that cannot be written must report the failure, not swallow it")
	}
	got := stable.LoadSnapshot(path, zerolog.Nop())
	if got == nil || !bytes.Equal(got.Payload, good.Payload) {
		t.Errorf("the failed write destroyed the previous snapshot: %+v", got)
	}
	assertOnlyFile(t, dir, "stable.json")
}

// assertOnlyFile fails when dir holds anything besides name -- a leftover temp
// file is the failure it is looking for.
func assertOnlyFile(t *testing.T, dir, name string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		got := make([]string, 0, len(entries))
		for _, e := range entries {
			got = append(got, e.Name())
		}
		t.Errorf("directory holds %v, want only %q: a temp file was left behind", got, name)
	}
}
