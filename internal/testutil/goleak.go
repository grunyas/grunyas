package testutil

import (
	"testing"

	"go.uber.org/goleak"
)

// VerifyTestMain wraps goleak.VerifyTestMain for reuse across packages.
func VerifyTestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
