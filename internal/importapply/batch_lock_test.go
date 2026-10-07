package importapply

import (
	"testing"
)

func TestAppendBatchLockKey(t *testing.T) {
	a := "abcdef00-0000-4000-8000-000000000001"
	b := "abcdef00-0000-4000-8000-000000000002"
	if batchLockKey(a, b) != batchLockKey("ABCDEF00-0000-4000-8000-000000000001", b) {
		t.Fatal("UUID spelling changes coordination")
	}
	if batchLockKey(a, b) == batchLockKey(b, a) {
		t.Fatal("tenant and request positions lost")
	}
}
