//go:build !linux && !darwin

package main

import (
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"os"
)

func openRegular(string) (*os.File, error)     { return nil, p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"} }
func verifyOpened(os.FileInfo, *os.File) error { return p.Failure{Code: "INPUT_TYPE_UNSUPPORTED"} }
