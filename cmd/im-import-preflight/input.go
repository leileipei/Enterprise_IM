package main

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/importinput"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"os"
)

type inputReader func(context.Context, string) ([]byte, *p.Issue, error)

func readInput(ctx context.Context, path string) ([]byte, *p.Issue, error) {
	return importinput.Read(ctx, path)
}
func openRegular(path string) (*os.File, error)          { return importinput.OpenRegular(path) }
func verifyOpened(initial os.FileInfo, f *os.File) error { return importinput.VerifyOpened(initial, f) }
