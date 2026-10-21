//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import "testing"

func TestCrossLogoutNavigationBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_LOGOUT_NAVIGATION", "1")
	TestCrossDeviceBackendFixture(t)
}
