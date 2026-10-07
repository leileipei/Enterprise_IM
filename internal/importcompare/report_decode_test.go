package importcompare

import (
	"bytes"
	"context"
	"testing"
)

func TestCompareUnitDecodeReport(t *testing.T) {
	r := BuildReport(validFile(), "valid", true, zeroClasses(), NewCollector())
	raw, err := EncodeReport(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReport(context.Background(), raw)
	if err != nil || decoded.ExitCode() != 0 {
		t.Fatal("typed report roundtrip")
	}
	for _, bad := range [][]byte{append(append([]byte{}, raw...), raw...), bytes.Replace(raw, []byte(`"status":"valid"`), []byte(`"status":"valid","status":"valid"`), 1), bytes.Replace(raw, []byte(`"status":"valid"`), []byte(`"status":"valid","secret":"marker"`), 1), bytes.Replace(raw, []byte(`"import_authorized":false`), []byte(`"import_authorized":true`), 1), bytes.Replace(raw, []byte(`"new":0`), []byte(`"new":0,"new":0`), 1), bytes.Replace(raw, []byte(`"new":0`), []byte(`"new":null`), 1), raw[:len(raw)-3]} {
		if _, err := DecodeReport(context.Background(), bad); err == nil {
			t.Fatal("malformed child report accepted")
		}
	}
}
