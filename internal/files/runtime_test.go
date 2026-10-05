package files

import (
	"errors"
	"testing"
	"time"
)

func TestFileRuntimeMeasurement(t *testing.T) {
	m := allocatedFile()
	for _, size := range []int64{1, 26214400} {
		m.DeclaredSizeBytes = size
		for _, typ := range []string{"application/pdf", "image/jpeg", "image/png", "text/plain", "application/octet-stream"} {
			if err := ValidateMeasurement(Measurement{SizeBytes: size, SHA256: [32]byte{1}, DetectedMediaType: typ}, m); err != nil {
				t.Fatal(size, typ, err)
			}
		}
	}
	m.DeclaredSizeBytes = 1
	for _, v := range []Measurement{{SizeBytes: 0, DetectedMediaType: "application/pdf"}, {SizeBytes: 2, DetectedMediaType: "application/pdf"}, {SizeBytes: 26214401, DetectedMediaType: "application/pdf"}, {SizeBytes: 1, DetectedMediaType: ""}, {SizeBytes: 1, DetectedMediaType: "text/plain; charset=utf-8"}, {SizeBytes: 1, DetectedMediaType: "application/zip"}} {
		if err := ValidateMeasurement(v, m); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("accepted %+v", v)
		}
	}
}
func runtimeTicket() UploadTicket {
	return UploadTicket{File: allocatedFile(), AttemptID: "10000000-0000-4000-8000-000000000071", LeaseToken: "10000000-0000-4000-8000-000000000072", OwnerID: "10000000-0000-4000-8000-000000000073", LeaseExpiresAt: fileTime.Add(180 * time.Second), Phase: UploadReceiving}
}
func TestFileRuntimePhases(t *testing.T) {
	measurement := Measurement{SizeBytes: 1, DetectedMediaType: "application/pdf"}
	for i := range measurement.SHA256 {
		measurement.SHA256[i] = 0xab
	}
	for _, phase := range []UploadPhase{UploadReceiving, UploadReceived, UploadStoring, UploadRecoveryPending, UploadRecovered, UploadSealed, UploadReceiveFailed} {
		t.Run(string(phase), func(t *testing.T) {
			v := runtimeTicket()
			v.Phase = phase
			if phase != UploadReceiving && phase != UploadReceiveFailed {
				v.Measurement = &measurement
			}
			if phase == UploadRecovered || phase == UploadSealed {
				v.ObjectVersionID = "opaque-V1"
			}
			if phase == UploadSealed {
				v.File = advanceFile(v.File, StateUploaded)
			}
			if err := ValidateUploadTicket(v); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*UploadTicket)
	}{
		{"unknown phase", func(v *UploadTicket) { v.Phase = "anything" }},
		{"empty attempt", func(v *UploadTicket) { v.AttemptID = "" }},
		{"bad owner", func(v *UploadTicket) { v.OwnerID = "worker" }},
		{"bad token", func(v *UploadTicket) { v.LeaseToken = "" }},
		{"zero lease", func(v *UploadTicket) { v.LeaseExpiresAt = time.Time{} }},
		{"lease before creation", func(v *UploadTicket) { v.LeaseExpiresAt = v.File.CreatedAt }},
		{"lease beyond ttl", func(v *UploadTicket) { v.LeaseExpiresAt = v.File.UploadExpiresAt.Add(time.Nanosecond) }},
		{"receiving measured", func(v *UploadTicket) { v.Measurement = &measurement }},
		{"receiving version", func(v *UploadTicket) { v.ObjectVersionID = "version" }},
		{"received no measurement", func(v *UploadTicket) { v.Phase = UploadReceived }},
		{"received version", func(v *UploadTicket) {
			v.Phase = UploadReceived
			v.Measurement = &measurement
			v.ObjectVersionID = "version"
		}},
		{"recovered no version", func(v *UploadTicket) { v.Phase = UploadRecovered; v.Measurement = &measurement }},
		{"null version", func(v *UploadTicket) {
			v.Phase = UploadRecovered
			v.Measurement = &measurement
			v.ObjectVersionID = "null"
		}},
		{"version control", func(v *UploadTicket) {
			v.Phase = UploadRecovered
			v.Measurement = &measurement
			v.ObjectVersionID = "v\n"
		}},
		{"seal allocated", func(v *UploadTicket) {
			v.Phase = UploadSealed
			v.Measurement = &measurement
			v.ObjectVersionID = "version"
		}},
		{"different size", func(v *UploadTicket) { v.Phase = UploadReceived; m := measurement; m.SizeBytes = 2; v.Measurement = &m }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := runtimeTicket()
			tc.edit(&v)
			if !errors.Is(ValidateUploadTicket(v), ErrInvalidMetadata) {
				t.Fatal("invalid ticket accepted")
			}
		})
	}
}

func TestFileRuntimeSealedEvidenceMatches(t *testing.T) {
	for _, field := range []string{"sha", "version", "type"} {
		t.Run(field, func(t *testing.T) {
			v := runtimeTicket()
			v.File = advanceFile(v.File, StateUploaded)
			v.Phase = UploadSealed
			v.ObjectVersionID = v.File.ObjectVersionID
			v.Measurement = &Measurement{SizeBytes: 1, DetectedMediaType: "application/pdf"}
			for i := range v.Measurement.SHA256 {
				v.Measurement.SHA256[i] = 0xab
			}
			switch field {
			case "sha":
				v.Measurement.SHA256[0] = 0
			case "version":
				v.ObjectVersionID = "different-version"
			case "type":
				v.Measurement.DetectedMediaType = "image/png"
			}
			if !errors.Is(ValidateUploadTicket(v), ErrInvalidMetadata) {
				t.Fatal("seal ticket diverged from immutable file evidence")
			}
		})
	}
}
