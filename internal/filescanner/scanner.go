package filescanner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"os"
	"sync"
	"time"
)

type Config struct{ QPDFPath, ClamdSocket, RuntimeManifestPath string }
type Scanner struct {
	config   Config
	mu       sync.Mutex
	proven   [32]byte
	verified bool
}

func New(c Config) (*Scanner, error) {
	if c.QPDFPath == "" || c.ClamdSocket == "" || c.RuntimeManifestPath == "" {
		return nil, ErrRuntimeUnavailable
	}
	if _, e := readManifest(c); e != nil {
		return nil, e
	}
	return &Scanner{config: c}, nil
}
func (s *Scanner) evidence(ctx context.Context) (RuntimeEvidence, [32]byte, error) {
	return attestRuntime(ctx, s.config)
}
func (s *Scanner) ValidateRuntime(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, stamp, err := s.evidence(ctx)
	if err != nil {
		return err
	}
	if err = validateRuntimeEvidence(e, time.Now()); err != nil {
		return err
	}
	if s.verified && stamp == s.proven {
		return nil
	}
	s.verified = false
	if err = runtimeProbes(ctx, s.config); err != nil {
		return err
	}
	_, after, err := s.evidence(ctx)
	if err != nil || stamp != after {
		return ErrRuntimeUnavailable
	}
	s.proven = stamp
	s.verified = true
	return nil
}
func (s *Scanner) Scan(parent context.Context, f *os.File, m files.Metadata) (files.ScanDecision, error) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	if err := s.ValidateRuntime(ctx); err != nil {
		return failedScan("scanner_unavailable"), err
	}
	evidence, before, err := s.evidence(ctx)
	if err != nil {
		return failedScan("scanner_unavailable"), err
	}
	s.mu.Lock()
	proven := s.verified && before == s.proven
	s.mu.Unlock()
	if !proven || validateRuntimeEvidence(evidence, time.Now()) != nil {
		return failedScan("scanner_unavailable"), ErrRuntimeUnavailable
	}
	if f == nil || len(m.SHA256) != 32 {
		return failedScan("object_mismatch"), ErrStructure
	}
	if _, err = f.Seek(0, 0); err != nil {
		return failedScan("object_read_failed"), err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, files.MaxFileSizeBytes+1))
	if err != nil || n != m.DeclaredSizeBytes || !bytes.Equal(h.Sum(nil), m.SHA256) {
		return failedScan("object_mismatch"), ErrStructure
	}
	d, err := checkStructure(ctx, f, m, s.config.QPDFPath)
	if err != nil {
		return failedScan("structure_invalid"), err
	}
	if d.State != files.StateRejected {
		d, err = scanClamd(ctx, s.config.ClamdSocket, f, m.DeclaredSizeBytes)
		if err != nil {
			return d, err
		}
		d.Engine = evidence.EngineVersion
		d.DefinitionVersion = evidence.DefinitionVersion
		copy(d.SHA256[:], m.SHA256)
	}
	_, after, err := s.evidence(ctx)
	if err != nil || before != after || ctx.Err() != nil {
		return failedScan("scanner_unavailable"), ErrRuntimeUnavailable
	}
	return d, nil
}
