package receiptoutbox

import (
	"context"
	"errors"
	"testing"
)

func TestReceiptOutboxUnconfigured(t *testing.T) {
	if processed, e := (Worker{}).ProcessOne(context.Background()); processed || !errors.Is(e, ErrWorkerUnconfigured) {
		t.Fatal(processed, e)
	}
}
