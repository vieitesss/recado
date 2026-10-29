package store

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
)

// AppendOutbox stores an upstream envelope as a complete line under flock.
func (s *Store) AppendOutbox(env envelope.Envelope) error {
	line, err := env.Line()
	if err != nil {
		return err
	}
	f, err := LockFile(filepath.Join(s.root, "outbox.jsonl"), os.O_RDWR|os.O_APPEND|os.O_CREATE, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer Unlock(f)
	if err := TrimTail(f); err != nil {
		return err
	}
	_, err = f.Write(line)
	return err
}

// ErrInvalidOutboxOffset means an offset is not a complete record boundary.
var ErrInvalidOutboxOffset = errors.New("invalid outbox offset")

// OutboxRecord carries an envelope and the byte position immediately after it.
type OutboxRecord struct {
	Offset   int64             `json:"offset"`
	Envelope envelope.Envelope `json:"envelope"`
}

// ReadOutbox reads complete records after a byte offset, filtering by host.
// The returned position includes records addressed to other hosts.
func (s *Store) ReadOutbox(after int64, host string) ([]OutboxRecord, int64, error) {
	if after < 0 {
		return nil, after, fmt.Errorf("%w: %d is negative", ErrInvalidOutboxOffset, after)
	}
	path := filepath.Join(s.root, "outbox.jsonl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if after == 0 {
			return nil, 0, nil
		}
		return nil, after, fmt.Errorf("%w: %d beyond end", ErrInvalidOutboxOffset, after)
	}
	if err != nil {
		return nil, after, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, after, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	info, err := f.Stat()
	if err != nil {
		return nil, after, err
	}
	if after > info.Size() {
		return nil, after, fmt.Errorf("%w: %d beyond end", ErrInvalidOutboxOffset, after)
	}
	if after > 0 {
		var b [1]byte
		if _, err := f.ReadAt(b[:], after-1); err != nil {
			return nil, after, err
		}
		if b[0] != '\n' {
			return nil, after, fmt.Errorf("%w: %d is not a line boundary", ErrInvalidOutboxOffset, after)
		}
	}
	if _, err := f.Seek(after, io.SeekStart); err != nil {
		return nil, after, err
	}
	r := bufio.NewReader(f)
	pos := after
	var records []OutboxRecord
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return records, pos, nil
		}
		if err != nil {
			return nil, pos, err
		}
		pos += int64(len(line))
		env, err := envelope.Decode(line)
		if err != nil {
			return nil, pos, fmt.Errorf("outbox at %d: %w", pos-int64(len(line)), err)
		}
		to, err := addr.Parse(env.To)
		if err != nil || !to.Upstream {
			return nil, pos, fmt.Errorf("invalid outbox recipient %q", env.To)
		}
		if to.Machine == host {
			records = append(records, OutboxRecord{Offset: pos, Envelope: env})
		}
	}
}
