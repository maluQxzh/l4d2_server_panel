package agent

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RotatingLog struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	maxAge   time.Duration
	file     *os.File
}

func NewRotatingLog(path string, maxBytes int64, backups int) (*RotatingLog, error) {
	return NewRotatingLogWithRetention(path, maxBytes, backups, 0)
}

func NewRotatingLogWithRetention(path string, maxBytes int64, backups int, maxAge time.Duration) (*RotatingLog, error) {
	if maxBytes <= 0 || backups < 0 || maxAge < 0 {
		return nil, fmt.Errorf("invalid log retention configuration")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	r := &RotatingLog{path: path, maxBytes: maxBytes, backups: backups, maxAge: maxAge}
	if err := r.open(); err != nil {
		return nil, err
	}
	if err := r.enforceAgeLocked(time.Now()); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (r *RotatingLog) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	r.file = f
	return nil
}

func (r *RotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return 0, os.ErrClosed
	}
	if err := r.enforceAgeLocked(time.Now()); err != nil {
		return 0, err
	}
	if st, err := r.file.Stat(); err == nil && st.Size()+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	return r.file.Write(p)
}

func (r *RotatingLog) enforceAgeLocked(now time.Time) error {
	if r.maxAge <= 0 {
		return nil
	}
	cutoff := now.Add(-r.maxAge)
	if st, err := r.file.Stat(); err != nil {
		return err
	} else if st.Size() > 0 && st.ModTime().Before(cutoff) {
		if err := r.rotateLocked(); err != nil {
			return err
		}
	}
	for index := 1; index <= r.backups; index++ {
		path := fmt.Sprintf("%s.%d", r.path, index)
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode().IsRegular() && st.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (r *RotatingLog) rotateLocked() error {
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	if r.backups > 0 {
		_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.backups))
		for i := r.backups - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
		}
		_ = os.Rename(r.path, r.path+".1")
	} else {
		_ = os.Remove(r.path)
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

func NewAgentLogger(dataDir string) (*log.Logger, io.Closer, error) {
	w, err := NewRotatingLog(filepath.Join(dataDir, "logs", "agent.log"), 10<<20, 3)
	if err != nil {
		return nil, nil, err
	}
	return log.New(io.MultiWriter(os.Stderr, w), "agent ", log.LstdFlags|log.LUTC|log.Lmicroseconds), w, nil
}

type lineSink struct {
	stream string
	log    io.Writer
	emit   func(stream, line string, at time.Time)
	mu     sync.Mutex
	buf    strings.Builder
}

const maxBufferedLogLine = 64 << 10

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.buf.Write(p)
	var writeErr error
	for {
		text := s.buf.String()
		idx := strings.IndexByte(text, '\n')
		if idx < 0 && len(text) <= maxBufferedLogLine {
			break
		}
		consume := idx + 1
		continuation := false
		if idx < 0 || idx > maxBufferedLogLine {
			idx, consume, continuation = maxBufferedLogLine, maxBufferedLogLine, true
		}
		line := strings.TrimSuffix(text[:idx], "\r")
		if continuation {
			line += "…[continued]"
		}
		rest := text[consume:]
		s.buf.Reset()
		_, _ = s.buf.WriteString(rest)
		at := time.Now().UTC()
		if s.log != nil {
			_, writeErr = fmt.Fprintf(s.log, "%s\t%s\t%s\n", at.Format(time.RFC3339Nano), s.stream, line)
		}
		if s.emit != nil {
			s.emit(s.stream, line, at)
		}
		if writeErr != nil {
			return len(p), writeErr
		}
	}
	return len(p), nil
}

func (s *lineSink) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() > 0 {
		at := time.Now().UTC()
		line := s.buf.String()
		if s.log != nil {
			_, _ = fmt.Fprintf(s.log, "%s\t%s\t%s\n", at.Format(time.RFC3339Nano), s.stream, line)
		}
		if s.emit != nil {
			s.emit(s.stream, line, at)
		}
	}
	s.buf.Reset()
}

func tailLines(path string, maxLines int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lines := make([]string, 0, maxLines)
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		if len(lines) == maxLines {
			copy(lines, lines[1:])
			lines[len(lines)-1] = s.Text()
		} else {
			lines = append(lines, s.Text())
		}
	}
	return lines, s.Err()
}
