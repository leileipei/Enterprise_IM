package importpreflight

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func emptyInput() string {
	return `{"format_version":1,"baseline_commit":"80424dbd5a537023c33e56654f4a12b522885a21","data_origin":"synthetic","reference_time":"2026-10-06T00:00:00+08:00","identity_source_selected":false,"tables":{"tenants":[],"legal_entities":[],"organizations":[],"departments":[],"users":[],"user_organizations":[],"user_departments":[],"external_identities":[],"admin_grants":[]}}`
}
func decoded(t *testing.T, s string) (RawDocument, bool, Report) {
	t.Helper()
	c := NewCollector()
	d, ok, e := DecodeRaw(context.Background(), []byte(s), c)
	if e != nil {
		t.Fatal(e)
	}
	return d, ok, BuildReport(Outcome{Status: "invalid"}, c)
}
func TestDecodeStrictKeysAndEnvelope(t *testing.T) {
	for _, tc := range []struct {
		s    string
		code Code
	}{
		{`{"format_version":1,"format_version":1}`, "DUPLICATE_JSON_KEY"},
		{strings.Replace(emptyInput(), `"users":[]`, `"users":[{"id":"x","\u0069d":"y"}]`, 1), "DUPLICATE_JSON_KEY"},
		{strings.Replace(emptyInput(), `"format_version":1`, `"secret-key":1,"format_version":1`, 1), "UNKNOWN_FIELD"},
		{strings.Replace(emptyInput(), `"users":[]`, `"secret-table":[],"users":[]`, 1), "UNKNOWN_TABLE"},
		{strings.Replace(emptyInput(), `"users":[]`, `"users":[{"secret-field":"private"}]`, 1), "UNKNOWN_FIELD"},
		{strings.Replace(emptyInput(), `"users":[],`, "", 1), "FIELD_REQUIRED"},
		{strings.Replace(emptyInput(), `"users":[]`, `"users":null`, 1), "TYPE_INVALID"},
		{strings.Replace(emptyInput(), `"users":[]`, `"users":{}`, 1), "TYPE_INVALID"},
		{strings.Replace(emptyInput(), `"users":[]`, `"users":[false]`, 1), "TYPE_INVALID"},
		{emptyInput() + ` {}`, "JSON_INVALID"}, {`{1:2}`, "JSON_INVALID"},
	} {
		_, ok, r := decoded(t, tc.s)
		if ok || !hasCode(r, tc.code) {
			t.Fatalf("expected %s: %+v", tc.code, r.Issues)
		}
	}
	_, ok, r := decoded(t, " \n"+emptyInput()+" \t")
	if !ok || r.ErrorsTotal != 0 {
		t.Fatal("valid envelope rejected")
	}
}
func TestDecodeUnicodeLexemes(t *testing.T) {
	for _, raw := range [][]byte{append([]byte{0xef, 0xbb, 0xbf}, []byte(emptyInput())...), []byte(strings.Replace(emptyInput(), "synthetic", string([]byte{0xff}), 1)), []byte(strings.Replace(emptyInput(), "synthetic", `\ud800`, 1)), []byte(strings.Replace(emptyInput(), "synthetic", `\udc00`, 1)), []byte(strings.Replace(emptyInput(), "synthetic", `\ud800\u0061`, 1))} {
		_, ok, r := decoded(t, string(raw))
		if ok || !hasCode(r, "INVALID_ENCODING") {
			t.Fatal("invalid Unicode accepted")
		}
	}
	_, ok, r := decoded(t, strings.Replace(emptyInput(), "synthetic", `\ud83d\ude00`, 1))
	if !ok || r.ErrorsTotal != 0 {
		t.Fatal("paired surrogate rejected")
	}
}
func TestDecodeDepthAndRowBudget(t *testing.T) {
	for _, n := range []int{16, 17} {
		s := strings.Repeat("[", n) + "0" + strings.Repeat("]", n)
		_, ok, r := decoded(t, s)
		if ok || hasCode(r, "DEPTH_LIMIT") != (n == 17) {
			t.Fatalf("depth %d %+v", n, r.Issues)
		}
		s = strings.Replace(emptyInput(), `"format_version":1`, `"hidden":`+s+`,"format_version":1`, 1)
		_, _, r = decoded(t, s)
		if !hasCode(r, "DEPTH_LIMIT") {
			t.Fatal("unknown deep structure bypass")
		}
	}
	for _, n := range []int{10000, 10001} {
		rows := strings.Repeat("{},", n-1) + "{}"
		_, ok, r := decoded(t, strings.Replace(emptyInput(), `"users":[]`, `"users":[`+rows+`]`, 1))
		if ok != (n == 10000) || hasCode(r, "ROW_LIMIT") != (n == 10001) {
			t.Fatalf("row %d %+v", n, r.Issues)
		}
	}
}
func TestDecodeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, e := DecodeRaw(ctx, []byte(emptyInput()), NewCollector())
	if ok || e == nil || e.Error() != "CANCELED" {
		t.Fatal("cancellation ignored")
	}
}
func TestDecodePreservesScalarLexemes(t *testing.T) {
	for _, v := range []string{"null", "1.0", "true", `"1"`, `[]`, `{}`} {
		d, ok, _ := decoded(t, strings.Replace(emptyInput(), `"format_version":1`, `"format_version":`+v, 1))
		if !ok || string(d.Header["format_version"]) != v {
			t.Fatal("lexeme replaced")
		}
	}
	d, ok, _ := decoded(t, emptyInput())
	if !ok || !json.Valid(d.Header["reference_time"]) {
		t.Fatal("lost scalar")
	}
}
