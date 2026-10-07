//go:build linux || darwin

package importinput

import (
	"errors"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"golang.org/x/sys/unix"
	"os"
	"strings"
)

func openError(e error) error {
	if errors.Is(e, unix.ELOOP) || errors.Is(e, unix.ENOTDIR) || errors.Is(e, unix.EISDIR) {
		return p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"}
	}
	return p.Failure{Code: "INPUT_READ_FAILED"}
}
func VerifyOpened(initial os.FileInfo, f *os.File) error {
	opened, e := f.Stat()
	if e != nil {
		return p.Failure{Code: "INPUT_READ_FAILED"}
	}
	if !initial.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(initial, opened) || initial.Mode() != opened.Mode() || initial.Size() != opened.Size() || !initial.ModTime().Equal(opened.ModTime()) {
		return p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"}
	}
	return nil
}
func OpenRegular(path string) (*os.File, error) {
	initial, e := os.Lstat(path)
	if e != nil {
		return nil, openError(e)
	}
	if !initial.Mode().IsRegular() {
		return nil, p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"}
	}
	root := "."
	if strings.HasPrefix(path, "/") {
		root = "/"
	}
	dir, e := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, openError(e)
	}
	defer func() { unix.Close(dir) }()
	parts := strings.Split(path, "/")
	last := len(parts) - 1
	if parts[last] == "" {
		return nil, p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"}
	}
	for _, part := range parts[:last] {
		if part == "" {
			continue
		}
		next, e := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return nil, openError(e)
		}
		unix.Close(dir)
		dir = next
	}
	fd, e := unix.Openat(dir, parts[last], unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, openError(e)
	}
	f := os.NewFile(uintptr(fd), "preflight-input")
	if e = VerifyOpened(initial, f); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
