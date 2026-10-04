package filescanner

import (
	"time"
)

type RuntimeEvidence struct {
	ConfigSHA256                                             [32]byte
	EngineVersion, DefinitionVersion                         string
	DefinitionsUpdatedAt                                     time.Time
	StreamMaxLengthBytes, MaxFileSizeBytes, MaxScanSizeBytes int64
	AlertExceedsMax, AlertEncrypted, AlertBroken             bool
}

func validateRuntimeEvidence(e RuntimeEvidence, now time.Time) error {
	if e.EngineVersion == "" || e.DefinitionVersion == "" || e.StreamMaxLengthBytes != 26214400 || e.MaxFileSizeBytes != 26214400 || e.MaxScanSizeBytes != 262144000 || !e.AlertExceedsMax || !e.AlertEncrypted || !e.AlertBroken {
		return ErrRuntimeUnavailable
	}
	if e.DefinitionsUpdatedAt.IsZero() || e.DefinitionsUpdatedAt.After(now) || now.Sub(e.DefinitionsUpdatedAt) > 24*time.Hour {
		return ErrDefinitionsStale
	}
	return nil
}
