package webclient

import "testing"

func TestWebFileTransfer(t *testing.T) { runFileUnit(t, "file-transfer", "e2e/file_transfer_unit.cjs") }
