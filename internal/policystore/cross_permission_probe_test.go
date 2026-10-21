//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import "testing"

// Only fixture selection changes. Permission ending goes through production API.
func TestCrossPermissionBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_PERMISSION_CHANGE", "1")
	TestCrossDeviceBackendFixture(t)
}
