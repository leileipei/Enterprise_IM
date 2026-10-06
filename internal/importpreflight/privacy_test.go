package importpreflight

import (
	"bytes"
	"context"
	"testing"
)

func TestPreflightPrivacyAndDeterminism(t *testing.T) {
	marker := "private-person-issuer-subject-name-marker"
	b := mutateBytes(t, sampleBytes(t), func(m map[string]any) {
		m["data_origin"] = marker
		for _, v := range tableRows(m, "users") {
			v.(map[string]any)["display_name"] = marker
		}
		for _, v := range tableRows(m, "external_identities") {
			v.(map[string]any)["issuer"] = marker
			v.(map[string]any)["subject"] = marker
		}
	})
	r := Evaluate(context.Background(), b)
	a, e := EncodeReport(r)
	if e != nil || bytes.Contains(a, []byte(marker)) {
		t.Fatal("private values leaked")
	}
	again, _ := EncodeReport(Evaluate(context.Background(), b))
	if !bytes.Equal(a, again) {
		t.Fatal("unstable private report")
	}
	b = mutateBytes(t, b, func(m map[string]any) { tableRows(m, "users")[0].(map[string]any)[marker] = marker })
	a, _ = EncodeReport(Evaluate(context.Background(), b))
	if bytes.Contains(a, []byte(marker)) || !hasCode(Evaluate(context.Background(), b), "UNKNOWN_FIELD") {
		t.Fatal("unknown name leaked or accepted")
	}
}
