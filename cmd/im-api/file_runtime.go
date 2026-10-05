package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type fileRuntime struct {
	business      fileBusinessConfig
	uploadEnabled bool
	transfer      *filetransfer.Service
	download      *filedownload.Service
	reader        objectstore.ReadOnlyStore
}
type apiPinger func(context.Context) error

func (p apiPinger) Ping(ctx context.Context) error { return p(ctx) }
func startFileRuntime(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, uploadEnabled bool, uploadObjects objectstore.Config, uploadSpool string, business fileBusinessConfig) (rt *fileRuntime, err error) {
	rt = &fileRuntime{business: business, uploadEnabled: uploadEnabled}
	if !uploadEnabled && !business.Enabled {
		return rt, nil
	}
	if ctx.Err() != nil || pool == nil {
		return nil, files.ErrDependencyUnavailable
	}
	if business.Enabled {
		for _, other := range []string{uploadSpool, getenv("IM_FILE_SPOOL_DIR"), getenv("IM_FILE_WORKER_SPOOL_DIR")} {
			if e := checkDownloadSpoolIsolation(business.SpoolDir, other); e != nil {
				return nil, files.ErrDependencyUnavailable
			}
		}
	}
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	defer func() {
		if err != nil {
			_ = rt.Close()
			rt = nil
			err = files.ErrDependencyUnavailable
		}
	}()
	repo := policystore.Service{DB: pool}
	if business.Enabled {
		if err = repo.CheckFileBusinessRuntime(initCtx); err != nil {
			return rt, err
		}
		if rt.reader, err = objectstore.NewS3ReadOnly(business.Objects); err != nil {
			return rt, err
		}
		if err = rt.reader.ValidateCapabilities(initCtx); err != nil {
			return rt, err
		}
		if err = rt.reader.ValidateReadProbe(initCtx, business.ProbeVersionID); err != nil {
			return rt, err
		}
		if initCtx.Err() != nil {
			return rt, initCtx.Err()
		}
		if rt.download, err = filedownload.NewService(repo, rt.reader, business.SpoolDir, business.OwnerID); err != nil {
			return rt, err
		}
	}
	if uploadEnabled {
		checkCtx, stop := context.WithTimeout(initCtx, 15*time.Second)
		defer stop()
		if err = pool.Ping(checkCtx); err != nil {
			return rt, err
		}
		var installed bool
		err = pool.QueryRow(checkCtx, "SELECT to_regclass('file_objects') IS NOT NULL AND to_regclass('tenant_file_upload_policy') IS NOT NULL AND to_regclass('file_upload_attempts') IS NOT NULL").Scan(&installed)
		if err != nil || !installed {
			return rt, files.ErrDependencyUnavailable
		}
		if business.Enabled {
			if err = repo.CheckFileBusinessUploadRuntime(checkCtx); err != nil {
				return rt, err
			}
		}
		var objects objectstore.Store
		if objects, err = objectstore.NewS3(uploadObjects); err != nil {
			return rt, err
		}
		if err = objects.ValidateCapabilities(checkCtx); err != nil {
			return rt, err
		}
		if checkCtx.Err() != nil {
			return rt, checkCtx.Err()
		}
		var owner [16]byte
		if _, err = rand.Read(owner[:]); err != nil {
			return rt, err
		}
		owner[6] = (owner[6] & 15) | 64
		owner[8] = (owner[8] & 63) | 128
		ownerID := fmt.Sprintf("%x-%x-%x-%x-%x", owner[:4], owner[4:6], owner[6:8], owner[8:10], owner[10:])
		if rt.transfer, err = filetransfer.NewService(repo, objects, uploadSpool, ownerID); err != nil {
			return rt, err
		}
		if checkCtx.Err() != nil {
			return rt, checkCtx.Err()
		}
	}
	if initCtx.Err() != nil {
		return rt, initCtx.Err()
	}
	return rt, nil
}
func (rt *fileRuntime) CheckHealth(ctx context.Context) error {
	if rt == nil || ctx.Err() != nil {
		return files.ErrDependencyUnavailable
	}
	if !rt.business.Enabled {
		return nil
	}
	if rt.reader == nil || rt.download == nil {
		return files.ErrDependencyUnavailable
	}
	if rt.download.CheckHealth(ctx) != nil || rt.reader.ValidateCapabilities(ctx) != nil || rt.reader.ValidateReadProbe(ctx, rt.business.ProbeVersionID) != nil {
		return files.ErrDependencyUnavailable
	}
	return ctx.Err()
}
func (rt *fileRuntime) Close() error {
	if rt == nil {
		return nil
	}
	var result error
	if rt.transfer != nil {
		result = errors.Join(result, rt.transfer.Close())
	}
	if rt.download != nil {
		result = errors.Join(result, rt.download.Close())
	}
	return result
}

// Resolve existing ancestors too, so a nonexistent child under a symlink cannot
// bypass the ancestor check. No directories are created by this check.
func resolveSpoolPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", files.ErrDependencyUnavailable
	}
	clean := filepath.Clean(path)
	probe := clean
	var suffix []string
	for {
		if _, e := os.Lstat(probe); e == nil {
			resolved, e := filepath.EvalSymlinks(probe)
			if e != nil {
				return "", e
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", files.ErrDependencyUnavailable
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}
func checkDownloadSpoolIsolation(download, other string) error {
	if other == "" {
		return nil
	}
	a, e := resolveSpoolPath(download)
	if e != nil {
		return e
	}
	b, e := resolveSpoolPath(other)
	if e != nil {
		return e
	}
	if a == b || strings.HasPrefix(a, strings.TrimSuffix(b, string(os.PathSeparator))+string(os.PathSeparator)) || strings.HasPrefix(b, strings.TrimSuffix(a, string(os.PathSeparator))+string(os.PathSeparator)) {
		return files.ErrDependencyUnavailable
	}
	ai, ae := os.Stat(a)
	bi, be := os.Stat(b)
	if ae == nil && be == nil && os.SameFile(ai, bi) {
		return files.ErrDependencyUnavailable
	}
	return nil
}
