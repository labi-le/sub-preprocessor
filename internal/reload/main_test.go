package reload_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the package's tests under goleak so any goroutine that outlives
// a test (e.g. a watcher goroutine not joined on shutdown) fails the suite.
// The mihomo library starts two process-lifetime goroutines from package init
// (imported transitively via internal/stable); they are not per-test leaks.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction(
			"github.com/metacubex/mihomo/common/observable.(*Observable[...]).process"),
		goleak.IgnoreTopFunction(
			"github.com/metacubex/mihomo/tunnel/statistic.(*Manager).handle"),
	)
}
