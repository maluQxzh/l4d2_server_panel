package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingLog is a bounded, process-local log sink used by the native Windows
// Control service. Rotation is deliberately simple: one writer owns the file
// and keeps a fixed number of size-bounded backups.
type RotatingLog struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	file     *os.File
}

func NewRotatingLog(path string, maxBytes int64, backups int) (*RotatingLog, error) {
	if path == "" || maxBytes <= 0 || backups < 0 {
		return nil, errors.New("invalid control log configuration")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	r := &RotatingLog{path: path, maxBytes: maxBytes, backups: backups}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingLog) open() error {
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open control log: %w", err)
	}
	r.file = file
	return nil
}

func (r *RotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return 0, os.ErrClosed
	}
	if info, err := r.file.Stat(); err != nil {
		return 0, err
	} else if info.Size()+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	return r.file.Write(p)
}

func (r *RotatingLog) rotateLocked() error {
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	if r.backups == 0 {
		_ = os.Remove(r.path)
	} else {
		_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.backups))
		for index := r.backups - 1; index >= 1; index-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", r.path, index), fmt.Sprintf("%s.%d", r.path, index+1))
		}
		_ = os.Rename(r.path, r.path+".1")
	}
	return r.open()
}

func (r *RotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
