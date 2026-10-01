package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// Forward compatibility: a state.json written by a newer chottag may carry
// top-level or per-account keys this binary does not know. Load keeps them
// (State.extra, Account.extra) and every save writes them back unchanged, so
// an older binary running an Update cannot erase a newer one's fields.

type (
	stateFields   State
	accountFields Account
)

// A release that retires a field must delete its key from extra explicitly,
// or an older state.json carries it on as an unknown key forever.

// jsonKeys is the set of JSON keys t's fields declare, lowercased:
// encoding/json matches a key to a field case-insensitively, so "NoRotate"
// fills NoRotate and must not also be kept (and written back) as unknown.
func jsonKeys(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[strings.ToLower(name)] = true
	}
	return out
}

var (
	stateKeys   = jsonKeys(reflect.TypeOf(State{}))
	accountKeys = jsonKeys(reflect.TypeOf(Account{}))
)

// unknownKeys returns the members of the JSON object b whose keys are not in
// known; nil when there are none.
func unknownKeys(b []byte, known map[string]bool) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	for k, v := range all {
		if known[strings.ToLower(k)] {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[k] = v
	}
	return extra, nil
}

// withExtra appends extra's members to the JSON object b, in key order.
func withExtra(b []byte, extra map[string]json.RawMessage) ([]byte, error) {
	if len(extra) == 0 {
		return b, nil
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.Write(b[:len(b)-1]) // up to, not including, the closing brace
	for _, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(extra[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (st *State) UnmarshalJSON(b []byte) error {
	a := stateFields(*st)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := unknownKeys(b, stateKeys)
	if err != nil {
		return err
	}
	*st = State(a)
	st.extra = extra
	return nil
}

func (st State) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(stateFields(st))
	if err != nil {
		return nil, err
	}
	return withExtra(b, st.extra)
}

func (a *Account) UnmarshalJSON(b []byte) error {
	f := accountFields(*a)
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	extra, err := unknownKeys(b, accountKeys)
	if err != nil {
		return err
	}
	*a = Account(f)
	a.extra = extra
	return nil
}

func (a Account) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(accountFields(a))
	if err != nil {
		return nil, err
	}
	return withExtra(b, a.extra)
}
