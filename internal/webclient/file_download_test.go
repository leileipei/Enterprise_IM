package webclient

import "testing"

func TestWebFileDownload(t *testing.T) { runFileUnit(t, "file-download", "e2e/file_download_unit.cjs") }
