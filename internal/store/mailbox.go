// Package store owns recado's on-disk state: the agent registry and the
// mailboxes. Every mailbox write happens under an flock on its inbox file, so
// concurrent senders never interleave a message or deliver one twice.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
)

// Modes recado creates its state with.
const (
	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600
)

// MessageState is where a message is in its delivery path: stored until
// someone claims it for push or drain (queued), and read once its harness
// reports the turn started.
type MessageState string

const (
	StateStored MessageState = "stored"
	StateQueued MessageState = "queued"
	StateRead   MessageState = "read"
)

// Valid reports whether s is a state recado records.
func (s MessageState) Valid() bool {
	switch s {
	case StateStored, StateQueued, StateRead:
		return true
	}
	return false
}

// Errors callers map to exit code 2: the address or id they named does not
// exist or is not enough to identify one message.
var (
	ErrUnknownAgent = errors.New("unknown agent")
	ErrUnknownID    = errors.New("no such message")
	ErrAmbiguousID  = errors.New("ambiguous message id")
)

// Store is a recado state directory.
type Store struct {
	root string
	// write appends one receipt batch; a test replaces it to make the disk
	// fail partway through a claim. Nil writes the batch as it is.
	write func(f *os.File, data []byte) (int, error)
}

// DefaultRoot is ${RECADO_STATE_DIR}, else
// ${XDG_STATE_HOME:-~/.local/state}/recado.
func DefaultRoot() (string, error) {
	if dir := os.Getenv("RECADO_STATE_DIR"); dir != "" {
		return dir, nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "recado"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "recado"), nil
}

// Open returns the store rooted at root, creating it if needed and enforcing
// the 0700 directory mode recado promises.
func Open(root string) (*Store, error) {
	if err := ensureDir(root); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// ensureDir creates a directory recado owns and keeps its mode at DirMode.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, DirMode); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() == DirMode {
		return nil
	}
	return os.Chmod(path, DirMode)
}

// Root is the state directory used by this store.
func (s *Store) Root() string { return s.root }

// Message is one stored envelope, its current state, and the short id it has
// in its mailbox.
type Message struct {
	Envelope envelope.Envelope
	State    MessageState
	Short    string // the shortest suffix that names it in this mailbox
}

// Append stores env in agent id's mailbox, under the mailbox lock. Storing an
// envelope whose id is already in the mailbox changes nothing, so a retried
// deliver from the relay cannot deliver the same message twice.
func (s *Store) Append(id string, env envelope.Envelope) error {
	line, err := env.Line()
	if err != nil {
		return err
	}
	f, err := s.lockMailbox(id, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer Unlock(f)
	envs, err := readInbox(f)
	if err != nil {
		return err
	}
	for _, existing := range envs {
		if existing.ID == env.ID {
			return nil
		}
	}
	if err := TrimTail(f); err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		return err
	}
	return nil
}

// Messages returns agent id's mailbox in append order, with each message's
// current state.
func (s *Store) Messages(id string) ([]Message, error) {
	f, err := s.lockMailbox(id, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer Unlock(f)
	return s.messagesLocked(f, id)
}

// MailboxOptions selects a mailbox view and whether to claim its stored messages.
type MailboxOptions struct {
	// Meta requires every value to match; a missing key matches an empty value.
	Meta map[string]string
	// ExcludeRead omits read messages. StoredOnly omits all non-stored messages.
	ExcludeRead bool
	StoredOnly  bool
	// Claim marks the selected stored messages queued; otherwise this is a peek.
	Claim bool
}

// Mailbox returns matching messages in append order, with short ids unique in
// the whole mailbox. Claim marks all matching stored messages queued in one
// batch under the mailbox lock; an unsuccessful claim records none of them.
func (s *Store) Mailbox(id string, opts MailboxOptions) ([]Message, error) {
	how := syscall.LOCK_SH
	if opts.Claim {
		how = syscall.LOCK_EX
	}
	f, err := s.lockMailbox(id, how)
	if err != nil {
		return nil, err
	}
	defer Unlock(f)
	msgs, err := s.messagesLocked(f, id)
	if err != nil {
		return nil, err
	}
	var selected []Message
	var receipts []receipt
	ts := envelope.FormatTime(time.Now())
	for _, msg := range msgs {
		if (opts.ExcludeRead && msg.State == StateRead) || (opts.StoredOnly && msg.State != StateStored) {
			continue
		}
		match := true
		for k, v := range opts.Meta {
			if msg.Envelope.Meta[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if opts.Claim && msg.State == StateStored {
			receipts = append(receipts, receipt{ID: msg.Envelope.ID, State: StateQueued, TS: ts})
			msg.State = StateQueued
		}
		selected = append(selected, msg)
	}
	if err := s.appendReceipts(id, receipts); err != nil {
		return nil, err
	}
	return selected, nil
}

// ShortID is the shortest suffix of msgID, at least four characters, that
// names exactly one message in agent id's mailbox. It returns ErrUnknownID
// when the message is absent.
func (s *Store) ShortID(id, msgID string) (string, error) {
	msgs, err := s.Messages(id)
	if err != nil {
		return "", err
	}
	for _, msg := range msgs {
		if strings.EqualFold(msg.Envelope.ID, msgID) {
			return msg.Short, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrUnknownID, msgID)
}

// Resolve finds the one message in agent id's mailbox whose id ends with
// query, which may be a short id or a full ULID.
func (s *Store) Resolve(id, query string) (Message, error) {
	msgs, err := s.Messages(id)
	if err != nil {
		return Message{}, err
	}
	return resolve(msgs, query)
}

// ResolveAny finds the one message in any mailbox whose id ends with query.
// It is what `recado status` uses: the short id send printed belongs to the
// recipient's mailbox, not the sender's.
func (s *Store) ResolveAny(query string) (Message, string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "mail"))
	if errors.Is(err, os.ErrNotExist) {
		return Message{}, "", ErrUnknownID
	}
	if err != nil {
		return Message{}, "", err
	}
	var found []Message
	var mailbox string
	for _, e := range entries {
		if !e.IsDir() || !addr.ValidID(e.Name()) {
			continue
		}
		msg, err := s.Resolve(e.Name(), query)
		switch {
		case err == nil:
			found = append(found, msg)
			mailbox = e.Name()
		case errors.Is(err, ErrUnknownID):
		case errors.Is(err, ErrAmbiguousID):
			return Message{}, "", err
		default:
			return Message{}, "", err
		}
	}
	switch len(found) {
	case 0:
		return Message{}, "", ErrUnknownID
	case 1:
		return found[0], mailbox, nil
	default:
		return Message{}, "", ErrAmbiguousID
	}
}

// Release returns a claimed message to stored, for example after a rejected
// push, so a later drain retries it.
func (s *Store) Release(id, msgID string) error {
	f, err := s.lockMailbox(id, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer Unlock(f)
	msgs, err := s.messagesLocked(f, id)
	if err != nil {
		return err
	}
	msg, err := find(msgs, msgID)
	if err != nil {
		return err
	}
	if msg.State == StateStored {
		return nil
	}
	return s.appendReceipt(id, msgID, StateStored)
}

// MarkQueuedRead records read for every queued message: the harness reported
// the turn started.
func (s *Store) MarkQueuedRead(id string) error {
	f, err := s.lockMailbox(id, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer Unlock(f)
	msgs, err := s.messagesLocked(f, id)
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		if msg.State != StateQueued {
			continue
		}
		if err := s.appendReceipt(id, msg.Envelope.ID, StateRead); err != nil {
			return err
		}
	}
	return nil
}

func resolve(msgs []Message, query string) (Message, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Message{}, ErrUnknownID
	}
	var found []Message
	for _, m := range msgs {
		if strings.HasSuffix(strings.ToLower(m.Envelope.ID), q) {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return Message{}, ErrUnknownID
	case 1:
		return found[0], nil
	default:
		return Message{}, ErrAmbiguousID
	}
}

// lockMailbox opens id's inbox (creating the mailbox) and takes the mailbox
// lock: every read and write of the mailbox goes through it.
func (s *Store) lockMailbox(id string, how int) (*os.File, error) {
	dir, err := s.mailboxDir(id)
	if err != nil {
		return nil, err
	}
	return LockFile(filepath.Join(dir, "inbox.jsonl"), os.O_RDWR|os.O_APPEND|os.O_CREATE, how)
}

// LockDelivery takes id's delivery lock: the one lock that covers reading the
// recipient's state, claiming its stored messages and pushing them, so a turn
// boundary cannot interleave with a delivery. The caller must call release.
func (s *Store) LockDelivery(id string) (release func(), err error) {
	dir, err := s.mailboxDir(id)
	if err != nil {
		return nil, err
	}
	f, err := LockFile(filepath.Join(dir, "delivery.lock"), os.O_RDWR|os.O_CREATE, syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	return func() { Unlock(f) }, nil
}

// LockFile opens path at FileMode and takes how's flock on it. The caller must Unlock it.
func LockFile(path string, flag, how int) (*os.File, error) {
	f, err := os.OpenFile(path, flag, FileMode)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(FileMode); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

// mailboxDir creates id's mailbox directory and returns it.
func (s *Store) mailboxDir(id string) (string, error) {
	if !addr.ValidID(id) {
		return "", fmt.Errorf("bad agent id %q", id)
	}
	dir := filepath.Join(s.root, "mail", id)
	if err := ensureDir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Unlock releases a flock and closes its file.
func Unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// readInbox reads complete lines from the locked inbox. A torn trailing line
// is what a crashed writer leaves behind; it is not a message.
func readInbox(f *os.File) ([]envelope.Envelope, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	var envs []envelope.Envelope
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		env, err := envelope.Decode(sc.Bytes())
		if err != nil {
			return nil, fmt.Errorf("inbox line %d: %w", n, err)
		}
		envs = append(envs, env)
	}
	return envs, sc.Err()
}

// states reads the receipts of id's mailbox: the last state recorded per
// message wins. Reading is lenient: a state recado does not know is reported
// as recorded, so one bad receipt cannot make a mailbox unreadable.
func (s *Store) states(id string) (map[string]MessageState, error) {
	data, err := os.ReadFile(s.receiptsPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]MessageState{}, nil
	}
	if err != nil {
		return nil, err
	}
	// A crashed writer leaves a torn last line; it is not a receipt.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	states := map[string]MessageState{}
	for n, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r receipt
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("receipts line %d: %w", n+1, err)
		}
		states[r.ID] = r.State
	}
	return states, nil
}

// receipt is one state change in receipts.jsonl.
type receipt struct {
	ID    string       `json:"id"`
	State MessageState `json:"state"`
	TS    string       `json:"ts"`
}

// appendReceipt records one state change. The caller holds the mailbox lock.
func (s *Store) appendReceipt(id, msgID string, state MessageState) error {
	return s.appendReceipts(id, []receipt{{ID: msgID, State: state, TS: envelope.FormatTime(time.Now())}})
}

// appendReceipts records a batch of state changes in one append, or none at
// all: a partial write is truncated away, so a failed claim leaves no message
// queued with nobody to deliver it. The caller holds the mailbox lock.
func (s *Store) appendReceipts(id string, receipts []receipt) error {
	if len(receipts) == 0 {
		return nil
	}
	var data []byte
	for _, r := range receipts {
		if !r.State.Valid() {
			return fmt.Errorf("bad message state %q", r.State)
		}
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		data = append(append(data, line...), '\n')
	}
	f, err := os.OpenFile(s.receiptsPath(id), os.O_RDWR|os.O_APPEND|os.O_CREATE, FileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(FileMode); err != nil {
		return err
	}
	if err := TrimTail(f); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	before := info.Size()
	if _, err := s.writeReceipts(f, data); err != nil {
		if terr := f.Truncate(before); terr != nil {
			return fmt.Errorf("%w (and the partial receipt could not be removed: %v)", err, terr)
		}
		return err
	}
	return nil
}

// writeReceipts appends one receipt batch to f.
func (s *Store) writeReceipts(f *os.File, data []byte) (int, error) {
	if s.write == nil {
		return f.Write(data)
	}
	return s.write(f, data)
}

// TrimTail truncates f to its last complete line, so the next append never
// concatenates onto a record a crashed writer left torn. The caller must hold
// the file's corresponding lock exclusively.
func TrimTail(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	data := make([]byte, size)
	if _, err := f.ReadAt(data, 0); err != nil {
		return err
	}
	if data[size-1] == '\n' {
		return nil
	}
	return f.Truncate(int64(bytes.LastIndexByte(data, '\n') + 1))
}

// messagesLocked reads a mailbox the caller has already locked. It assembles
// every view of a mailbox: the envelopes, their states and their short ids.
func (s *Store) messagesLocked(f *os.File, id string) ([]Message, error) {
	envs, err := readInbox(f)
	if err != nil {
		return nil, err
	}
	states, err := s.states(id)
	if err != nil {
		return nil, err
	}
	msgs := make([]Message, len(envs))
	for i, env := range envs {
		state, ok := states[env.ID]
		if !ok {
			state = StateStored
		}
		msgs[i] = Message{Envelope: env, State: state}
	}
	// Count suffixes across the full mailbox, including messages a view may
	// later filter out, so every short id remains unambiguous in the mailbox.
	counts := make(map[string]int)
	for _, msg := range msgs {
		id := strings.ToLower(msg.Envelope.ID)
		for n := 4; n < len(id); n++ {
			counts[id[len(id)-n:]]++
		}
	}
	for i := range msgs {
		id := strings.ToLower(msgs[i].Envelope.ID)
		msgs[i].Short = id
		for n := 4; n < len(id); n++ {
			suffix := id[len(id)-n:]
			if counts[suffix] == 1 {
				msgs[i].Short = suffix
				break
			}
		}
	}
	return msgs, nil
}

// find returns the message with exactly this id.
func find(msgs []Message, msgID string) (Message, error) {
	for _, m := range msgs {
		if m.Envelope.ID == msgID {
			return m, nil
		}
	}
	return Message{}, fmt.Errorf("%w: %s", ErrUnknownID, msgID)
}

func (s *Store) receiptsPath(id string) string {
	return filepath.Join(s.root, "mail", id, "receipts.jsonl")
}
