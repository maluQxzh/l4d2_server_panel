//go:build !windows

package main

import (
	"errors"
	"io"
	"log/slog"
)

func runPortableCommand(_ *slog.Logger, _ []string, _ io.Reader, _ io.Writer) error {
	return errors.New("Windows portable management is supported only on Windows x64")
}
