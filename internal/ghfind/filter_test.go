package ghfind_test

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/ghfind"
)

func treeEntry(path string, size int) ghfind.TreeEntry {
	return ghfind.TreeEntry{Path: path, Size: size, Blob: true}
}

// TestCandidatesKeepsScoredTxt verifies the keep/drop decisions and the
// score-then-size ordering the guide states, on a tree mixing admissible
// blobs with the refusal classes: .yaml, a docs/ path, a 500-byte blob under
// the 2 KB floor with a low score, a README, an over-cap blob, and non-blob
// directory entries.
func TestCandidatesKeepsScoredTxt(t *testing.T) {
	t.Parallel()

	const (
		kb     = 1 << 10
		tiny   = 500
		tenMiB = 10 << 20
	)
	tree := []ghfind.TreeEntry{
		treeEntry("config/free.txt", 50*kb),   // score 9: config+free tokens
		treeEntry("sub/v2ray.txt", 5*kb),      // score 8
		treeEntry("proxy.txt", 30*kb),         // score 7
		treeEntry("nodes.txt", 25*kb),         // score 7, smaller than proxy.txt
		treeEntry("sub/list.txt", 12*kb),      // score 6: only the "sub" token
		treeEntry("plain.txt", 80*kb),         // score 5: no token
		treeEntry("config.yaml", 5*kb),        // refused: yaml extension
		treeEntry("docs/list.txt", 5*kb),      // refused: docs/ directory
		treeEntry("data/blob.txt", tiny),      // refused: under 2 KB, score 3 < waiver
		treeEntry("README.md", 10*kb),         // refused: extension and name
		treeEntry("huge.txt", tenMiB+1),       // refused: over the subscription cap
		treeEntry("node_modules/x.txt", 4*kb), // refused: vendoring directory
		{Path: "src", Size: 0, Blob: false},   // refused: not a blob
	}

	want := []string{
		"config/free.txt", // 9 first
		"sub/v2ray.txt",   // 8
		"proxy.txt",       // 7 with the larger size first
		"nodes.txt",       // 7
		"sub/list.txt",    // 6
		"plain.txt",       // 5
	}
	got := ghfind.Candidates(tree, len(want))
	if len(got) != len(want) {
		t.Fatalf("Candidates returned %d entries %+v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Path != w {
			t.Fatalf("entry %d = %q, want %q (order: score then size, descending)", i, got[i].Path, w)
		}
	}

	limited := ghfind.Candidates(tree, 3)
	if len(limited) != 3 || limited[0].Path != "config/free.txt" || limited[2].Path != "proxy.txt" {
		t.Fatalf("Candidates(tree, 3) = %+v, want the top three in order", limited)
	}
	if ghfind.Candidates(tree, 0) != nil {
		t.Fatal("Candidates with limit 0 must return nil")
	}
}

// TestCandidatesWaivesFloorForHighScore pins the small-file waiver: below the
// 2 KB floor only a score of at least the waiver threshold survives, so a
// strongly named tiny file is kept while a weakly named one of the same size
// is dropped.
func TestCandidatesWaivesFloorForHighScore(t *testing.T) {
	t.Parallel()

	tree := []ghfind.TreeEntry{
		treeEntry("sub/v2ray-free.txt", 600), // tokens sub+v2ray+free: score 9, kept despite size
		treeEntry("output.txt", 600),         // no token: score 3, dropped by the floor
	}
	got := ghfind.Candidates(tree, 4)
	if len(got) != 1 || got[0].Path != "sub/v2ray-free.txt" {
		t.Fatalf("Candidates = %+v, want only the high-scoring tiny file kept", got)
	}
}
