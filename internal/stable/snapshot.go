package stable

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// snapshotFileMode keeps the file private to the service uid: /stable.txt is
// the supported way to read the list, and the file is only a restart cache.
const snapshotFileMode os.FileMode = 0o600

// snapshotFile is the on-disk form of a Snapshot. Payload is a string rather
// than []byte because the list is UTF-8 text and encoding/json would base64 a
// byte slice, turning a document an operator can read into one they cannot.
type snapshotFile struct {
	Payload   string    `json:"payload"`
	UpdatedAt time.Time `json:"updated_at"`
	Stats     Stats     `json:"stats"`
}

// LoadSnapshot reads the list persisted by the last publish so /stable.txt can
// serve it from the first request. It never fails a startup: an empty path
// (persistence off), a missing file, an unreadable one and a malformed one all
// return nil, which leaves the holder empty and /stable.txt answering 503
// exactly as it did before this file existed.
//
// There is no TTL. That mirrors the in-memory rule -- the last good list is
// kept when a cycle fails -- so a restored list is never staler than what a run
// of failing cycles already serves, and its age stays visible in the
// X-Stable-Stats updated= field.
func LoadSnapshot(path string, logger zerolog.Logger) *Snapshot {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		logger.Warn().Err(err).Str("path", path).
			Msg("no stable snapshot restored; /stable.txt answers 503 until the first cycle completes")

		return nil
	}
	var f snapshotFile
	if err = json.Unmarshal(b, &f); err != nil {
		logger.Warn().Err(err).Str("path", path).
			Msg("stable snapshot is malformed; /stable.txt answers 503 until the first cycle completes")

		return nil
	}
	// A cycle never publishes an empty list (RunOnce skips the Store when no
	// survivor is left), so a file carrying one was not written by us, and
	// serving it would answer 200 with nothing in the body.
	if f.Payload == "" {
		logger.Warn().Str("path", path).
			Msg("stable snapshot carries an empty list; /stable.txt answers 503 until the first cycle completes")

		return nil
	}
	logger.Info().Str("path", path).Time("updated_at", f.UpdatedAt).Int("kept", f.Stats.Kept).
		Msg("restored stable list from snapshot")

	return &Snapshot{Payload: []byte(f.Payload), UpdatedAt: f.UpdatedAt, Stats: f.Stats}
}

// SaveSnapshot persists s, or does nothing when path is empty. The document is
// written to a temp file in the SAME directory and renamed over path, so a
// concurrent reader observes either the previous snapshot or this one, never a
// partial document, and a failed write leaves the previous one in place.
//
// The marshal copies the whole payload into the JSON document, and that copy is
// nearly all of what this function ALLOCATES -- the atomic write beside it is
// barely a kilobyte. It is paid once per PUBLISHED cycle -- tens of minutes
// apart, after a probe pass that has just dialled every node and allocated
// orders of magnitude more -- so hand-streaming the payload into the file to
// avoid it would trade a self-describing single document for a real saving
// that is irrelevant at this frequency.
func SaveSnapshot(path string, s *Snapshot) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(snapshotFile{
		Payload:   string(s.Payload),
		UpdatedAt: s.UpdatedAt,
		Stats:     s.Stats,
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	return writeSnapshotAtomic(path, b)
}

// writeSnapshotAtomic performs the temp-write/fsync/rename dance. The temp file
// is a sibling so the rename stays within one filesystem, and it is removed on
// every failure path -- after a successful rename that name is gone and the
// removal is a no-op.
func writeSnapshotAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, snapshotFileMode)
	if err != nil {
		return fmt.Errorf("open temp: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()

	if _, err = f.Write(b); err != nil {
		_ = f.Close()

		return fmt.Errorf("write temp: %w", err)
	}
	// The rename is what publishes the file; without the sync a host crash can
	// publish a name whose content never reached the device.
	if err = f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("sync temp: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}
