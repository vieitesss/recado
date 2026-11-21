package skill

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Hooks is the hook configuration recado installs in a harness's JSON settings
// file, beside whatever the user wrote there. Every command recado installs
// starts with Prefix, which is how Install, Inspect and Remove recognise
// recado's own entries: no other key, hook or value of the file is touched, and
// the keys keep their order.
type Hooks struct {
	Path    string      // the harness's settings file
	Prefix  string      // every command recado owns starts with it
	Entries []HookEntry // what recado installs, in order
	Section string      // top-level object holding the events; defaults to "hooks"
	Flat    bool        // events hold flat handler lists instead of matcher/hooks groups
}

// HookEntry is one hook recado installs: the event it fires on and its command.
type HookEntry struct {
	Event   string
	Command string
}

func (h Hooks) section() string {
	if h.Section != "" {
		return h.Section
	}
	return "hooks"
}

// Inspect reports whether Path holds recado's hook entries, and whether they
// are the ones this binary installs. Foreign never applies: the file belongs to
// the user, recado only adds entries to it.
func (h Hooks) Inspect() (State, error) {
	root, err := h.read()
	if err != nil {
		return 0, err
	}
	events, err := h.hooksOf(root)
	if err != nil {
		return 0, err
	}
	_, ours, err := h.stripOurs(events)
	if err != nil {
		return 0, err
	}
	if len(ours) == 0 {
		return Missing, nil
	}
	slices.SortFunc(ours, compareEntries)
	want := slices.Clone(h.Entries)
	slices.SortFunc(want, compareEntries)
	if slices.Equal(ours, want) {
		return Current, nil
	}
	return Outdated, nil
}

// Install adds recado's hook entries, replacing any entries an older recado
// installed. It refuses a settings file that is not valid JSON.
func (h Hooks) Install() error {
	root, events, err := h.editable()
	if err != nil {
		return err
	}
	if events, _, err = h.stripOurs(events); err != nil {
		return err
	}
	for _, entry := range h.Entries {
		if events, err = h.addEntry(events, entry); err != nil {
			return err
		}
	}
	return h.write(setKey(root, h.section(), events))
}

// Remove deletes recado's hook entries and nothing else. It reports whether it
// removed anything.
func (h Hooks) Remove() (bool, error) {
	root, events, err := h.editable()
	if err != nil {
		return false, err
	}
	ours, removed, err := h.stripOurs(events)
	if err != nil {
		return false, err
	}
	if len(removed) == 0 {
		return false, nil
	}
	return true, h.write(setKey(root, h.section(), ours))
}

// read decodes the settings file. A missing or empty file is an empty object:
// there are no settings to keep.
func (h Hooks) read() (object, error) {
	data, err := os.ReadFile(h.Path)
	if errors.Is(err, os.ErrNotExist) {
		return newObject(), nil
	}
	if err != nil {
		return object{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return newObject(), nil
	}
	root, err := decodeObject(data)
	if err != nil {
		return object{}, fmt.Errorf("%s: %w", h.Path, err)
	}
	return root, nil
}

// editable decodes the settings file and the "hooks" object inside it.
func (h Hooks) editable() (object, object, error) {
	root, err := h.read()
	if err != nil {
		return object{}, object{}, err
	}
	events, err := h.hooksOf(root)
	return root, events, err
}

// hooksOf is the hook-events object of a settings document: event name to the
// hook groups or handlers that fire on it.
func (h Hooks) hooksOf(root object) (object, error) {
	section := h.section()
	raw, ok := root.get(section)
	if !ok {
		return newObject(), nil
	}
	events, err := decodeObject(raw)
	if err != nil {
		return object{}, fmt.Errorf("%s: %s: %w", h.Path, section, err)
	}
	return events, nil
}

// write saves the settings document, and deletes the file when recado's entries
// were all it held.
func (h Hooks) write(root object) error {
	if root.len() == 0 {
		if err := os.Remove(h.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	compact, err := root.MarshalJSON()
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	return writeFile(h.Path, out.Bytes())
}

func (h Hooks) stripOurs(events object) (object, []HookEntry, error) {
	if h.Flat {
		return stripOursFlat(events, h.Prefix)
	}
	return stripOurs(events, h.Prefix)
}

func (h Hooks) addEntry(events object, entry HookEntry) (object, error) {
	if h.Flat {
		return addEntryFlat(events, entry)
	}
	return addEntry(events, entry)
}

// stripOursFlat removes every flat handler whose command starts with prefix,
// preserving non-array fields (such as "enabled") and other handlers.
func stripOursFlat(events object, prefix string) (object, []HookEntry, error) {
	kept := newObject()
	var removed []HookEntry
	for _, event := range events.keys {
		raw := bytes.TrimSpace(events.raw[event])
		if len(raw) == 0 || raw[0] != '[' {
			kept = kept.set(event, events.raw[event])
			continue
		}
		list, err := hookList(raw)
		if err != nil {
			return object{}, nil, fmt.Errorf("%s: %w", event, err)
		}
		var theirs []json.RawMessage
		for _, hook := range list {
			if command := hookCommand(hook); strings.HasPrefix(command, prefix) {
				removed = append(removed, HookEntry{Event: event, Command: command})
			} else {
				theirs = append(theirs, hook)
			}
		}
		if len(theirs) > 0 {
			kept = kept.set(event, marshal(hookListOf(theirs)))
		}
	}
	return kept, removed, nil
}

// addEntryFlat appends one flat command handler to its event.
func addEntryFlat(events object, entry HookEntry) (object, error) {
	var list []json.RawMessage
	if raw, ok := events.get(entry.Event); ok {
		var err error
		if list, err = hookList(raw); err != nil {
			return object{}, fmt.Errorf("%s: %w", entry.Event, err)
		}
	}
	handler := marshal(newObject().
		set("type", marshal("command")).
		set("command", marshal(entry.Command)))
	return events.set(entry.Event, marshal(hookListOf(append(list, handler)))), nil
}

// stripOurs removes every hook whose command starts with prefix, and returns
// the entries it removed. A group is dropped once it holds no hook at all; an
// event is dropped once it holds no group.
func stripOurs(events object, prefix string) (object, []HookEntry, error) {
	kept := newObject()
	var removed []HookEntry
	for _, event := range events.keys {
		list, err := hookList(events.raw[event])
		if err != nil {
			return object{}, nil, fmt.Errorf("%s: %w", event, err)
		}
		groups := make([]json.RawMessage, 0, len(list))
		for _, raw := range list {
			group, hooks, err := hookGroup(raw)
			if err != nil {
				return object{}, nil, fmt.Errorf("%s: %w", event, err)
			}
			var theirs []json.RawMessage
			var mine []string
			for _, hook := range hooks {
				if command := hookCommand(hook); strings.HasPrefix(command, prefix) {
					mine = append(mine, command)
				} else {
					theirs = append(theirs, hook)
				}
			}
			for _, command := range mine {
				removed = append(removed, HookEntry{Event: event, Command: command})
			}
			switch {
			case len(mine) == 0:
				groups = append(groups, raw) // not ours: keep it as it was
			case len(theirs) > 0:
				raw, err := group.set("hooks", marshal(hookListOf(theirs))).MarshalJSON()
				if err != nil {
					return object{}, nil, err
				}
				groups = append(groups, raw)
			}
		}
		if len(groups) > 0 {
			kept = kept.set(event, marshal(hookListOf(groups)))
		}
	}
	return kept, removed, nil
}

// addEntry appends one hook group for entry to its event.
func addEntry(events object, entry HookEntry) (object, error) {
	var list []json.RawMessage
	if raw, ok := events.get(entry.Event); ok {
		var err error
		if list, err = hookList(raw); err != nil {
			return object{}, fmt.Errorf("%s: %w", entry.Event, err)
		}
	}
	group := newObject().set("hooks", marshal(hookListOf([]json.RawMessage{
		marshal(newObject().
			set("type", marshal("command")).
			set("command", marshal(entry.Command))),
	})))
	raw, err := group.MarshalJSON()
	if err != nil {
		return object{}, err
	}
	return events.set(entry.Event, marshal(hookListOf(append(list, raw)))), nil
}

// hookList decodes one event's list of hook groups.
func hookList(raw json.RawMessage) ([]json.RawMessage, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("want a list of hooks: %w", err)
	}
	return list, nil
}

// hookGroup decodes one hook group: the object that carries a matcher and the
// hooks it runs.
func hookGroup(raw json.RawMessage) (object, []json.RawMessage, error) {
	group, err := decodeObject(raw)
	if err != nil {
		return object{}, nil, fmt.Errorf("hook group: %w", err)
	}
	hooks, ok := group.get("hooks")
	if !ok {
		return group, nil, nil
	}
	list, err := hookList(hooks)
	if err != nil {
		return object{}, nil, err
	}
	return group, list, nil
}

// hookCommand is the command line of one hook entry, empty when it has none.
func hookCommand(raw json.RawMessage) string {
	var hook struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &hook); err != nil {
		return ""
	}
	return hook.Command
}

// hookListOf encodes a list of hook groups.
func hookListOf(list []json.RawMessage) json.RawMessage {
	if list == nil {
		list = []json.RawMessage{}
	}
	return marshal(list)
}

func compareEntries(a, b HookEntry) int {
	if c := cmp.Compare(a.Event, b.Event); c != 0 {
		return c
	}
	return cmp.Compare(a.Command, b.Command)
}

// marshal encodes v, which for a settings value cannot fail.
func marshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err) // a marshalable settings value always encodes
	}
	return data
}

// object is a JSON object that keeps its keys in file order, so writing a
// settings file back leaves the rest of it where the user put it.
type object struct {
	keys []string
	raw  map[string]json.RawMessage
}

func newObject() object { return object{raw: map[string]json.RawMessage{}} }

func (o object) get(key string) (json.RawMessage, bool) {
	v, ok := o.raw[key]
	return v, ok
}

// set adds key at the end, or replaces the value it already has.
func (o object) set(key string, value json.RawMessage) object {
	if o.raw == nil {
		o.raw = map[string]json.RawMessage{}
	}
	if _, ok := o.raw[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.raw[key] = value
	return o
}

// setKey is set, or deletes the key when v is an empty object.
func setKey(o object, key string, v object) object {
	if v.len() == 0 {
		return o.del(key)
	}
	raw, err := v.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return o.set(key, raw)
}

// del removes key, leaving the order of the others alone.
func (o object) del(key string) object {
	if _, ok := o.raw[key]; !ok {
		return o
	}
	delete(o.raw, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
	return o
}

func (o object) len() int { return len(o.keys) }

// MarshalJSON writes the object compactly: json.Indent lays the whole document
// out afterwards, so raw values the caller kept are re-indented too.
func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		b.Write(encoded)
		b.WriteByte(':')
		b.Write(o.raw[key])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// decodeObject parses one JSON object, keeping its keys in order and every
// value as it stood in the file.
func decodeObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return object{}, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return object{}, errors.New("want a JSON object")
	}
	out := newObject()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return object{}, err
		}
		key, ok := tok.(string)
		if !ok {
			return object{}, errors.New("want a key")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return object{}, err
		}
		out = out.set(key, value)
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return object{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return object{}, errors.New("trailing data after the settings object")
	}
	return out, nil
}

// writeFile replaces path with data as one step, keeping the mode of the file
// it replaces.
func writeFile(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	tmp = nil
	return nil
}
