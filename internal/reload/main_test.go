package reload_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the package's tests under goleak so any goroutine that outlives
// a test (e.g. a watcher goroutine not joined on shutdown) fails the suite.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
