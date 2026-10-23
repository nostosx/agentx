package workflow

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
)

// State is the data shared by the nodes of a workflow run. Its map operations
// are safe for concurrent use. Values are shared without copying: treat them
// as immutable and replace them through Set or Update. Mutating a stored map,
// slice or pointer in place bypasses synchronization and staged writes.
//
// Inside a workflow run each node attempt works on its own view of the
// state: it reads the values committed so far together with its own
// writes, and its writes reach the shared state only when the node
// succeeds, all at once. Writes of a failed or suspended attempt are
// discarded, and nodes running in parallel do not see each other's writes
// until they finish. See Reducer for how the writes of parallel nodes are
// merged.
type State struct {
	mu     sync.RWMutex
	values map[string]any
	// view is set for the state a node attempt works on.
	view *view
}

// view records the writes of a node attempt over a base state.
type view struct {
	base     *State
	reducers map[string]ReducerFunc
	local    map[string]any
	deleted  map[string]bool
	ops      []op
	// private keys, such as a Map item and index, are visible to the node
	// but never committed.
	private map[string]bool
}

type opKind int

const (
	opSet opKind = iota
	opDelete
	// opUpdate replays an Update function on commit.
	opUpdate
	// opReplace sets a value without reducer or conflict check, for the
	// result of an Update that cannot be replayed, such as in a Map item.
	opReplace
)

// op is a write of a node attempt.
type op struct {
	kind  opKind
	key   string
	value any
	fn    func(map[string]any)
	// reduce is the reducer of the workflow that recorded a set, if it
	// declared one; it takes precedence over reducers further out.
	reduce ReducerFunc
	// diff holds the keys an Update changed on the view, so it can be
	// stored as plain writes; nil values with a deleted entry are removals.
	diff    map[string]any
	removed []string
}

// NewState returns a state holding a shallow copy of init.
func NewState(init map[string]any) *State {
	s := &State{values: make(map[string]any, len(init))}
	maps.Copy(s.values, init)
	return s
}

// newView returns a state for a node attempt over base.
func newView(base *State, reducers map[string]ReducerFunc) *State {
	return &State{view: &view{base: base, reducers: reducers, local: map[string]any{}, deleted: map[string]bool{}}}
}

// Get returns the value for key.
func (s *State) Get(key string) (any, bool) {
	s.mu.RLock()
	if v := s.view; v != nil {
		if val, ok := v.local[key]; ok {
			s.mu.RUnlock()
			return val, true
		}
		if v.deleted[key] {
			s.mu.RUnlock()
			return nil, false
		}
		s.mu.RUnlock()
		return v.base.Get(key)
	}
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

// String returns the value for key formatted as a string, or "".
func (s *State) String(key string) string {
	v, _ := s.Get(key)
	return format(v)
}

func format(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// Set stores a value. Inside a workflow node whose workflow declares a
// Reducer for key, value is an update that the reducer merges into the
// current value; Get then returns the merged value.
func (s *State) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.view; v != nil {
		v.record(s, op{kind: opSet, key: key, value: value})
		return
	}
	if s.values == nil {
		s.values = map[string]any{}
	}
	s.values[key] = value
}

// Delete removes a value.
func (s *State) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.view; v != nil {
		v.record(s, op{kind: opDelete, key: key})
		return
	}
	delete(s.values, key)
}

// Update calls fn with exclusive access to the state's map for read-modify-write
// operations. fn must replace mutable values rather than modify them in place,
// must not retain the map, and must not call other methods of the same State.
//
// Inside a workflow node, fn first runs on the node's view, so the node
// sees its effect, and runs again when the node's writes are committed,
// on the values current then. Updates of parallel nodes therefore compose
// instead of conflicting. fn must not have other side effects. In Map
// items, whose writes are stored per item, the values fn produced are
// committed instead, without running it again.
func (s *State) Update(fn func(values map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.view; v != nil {
		m := v.snapshot()
		before := maps.Clone(m)
		fn(m)
		o := op{kind: opUpdate, fn: fn, diff: map[string]any{}}
		for k, val := range m {
			if old, ok := before[k]; !ok || !reflect.DeepEqual(old, val) {
				o.diff[k] = val
				v.local[k] = val
				delete(v.deleted, k)
			}
		}
		for k := range before {
			if _, ok := m[k]; !ok {
				o.removed = append(o.removed, k)
				delete(v.local, k)
				v.deleted[k] = true
			}
		}
		v.ops = append(v.ops, o)
		return
	}
	if s.values == nil {
		s.values = map[string]any{}
	}
	fn(s.values)
}

// Snapshot returns a shallow copy of all values.
func (s *State) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v := s.view; v != nil {
		return v.snapshot()
	}
	out := make(map[string]any, len(s.values))
	maps.Copy(out, s.values)
	return out
}

// Decode converts the value for key into dst through JSON.
func (s *State) Decode(key string, dst any) (bool, error) {
	v, ok := s.Get(key)
	if !ok {
		return false, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return true, err
	}
	return true, json.Unmarshal(b, dst)
}

// Value returns the value for key as a T, converting through JSON as
// Decode does, so values restored from checkpoints, which are plain JSON
// values, read as the type they were stored as. It reports false if the
// state has no such value.
func (s *State) Value[T any](key string) (T, bool, error) {
	var v T
	ok, err := s.Decode(key, &v)
	return v, ok, err
}

// snapshot returns the base values with the view's writes. The caller
// holds the view's lock.
func (v *view) snapshot() map[string]any {
	out := v.base.Snapshot()
	for k := range v.deleted {
		delete(out, k)
	}
	maps.Copy(out, v.local)
	return out
}

// current returns the value of key as the view sees it. The caller holds
// the view's lock.
func (v *view) current(key string) (any, bool) {
	if val, ok := v.local[key]; ok {
		return val, true
	}
	if v.deleted[key] {
		return nil, false
	}
	return v.base.Get(key)
}

// reducer returns the reducer that applies to a write of key recorded
// with o.
func (v *view) reducer(o op) ReducerFunc {
	if o.reduce != nil {
		return o.reduce
	}
	return v.reducers[o.key]
}

// record applies o to the view and keeps it for the commit. The caller
// holds s's lock.
func (v *view) record(_ *State, o op) {
	switch o.kind {
	case opSet:
		val := o.value
		if r := v.reducer(o); r != nil {
			old, _ := v.current(o.key)
			val = r(old, o.value)
		}
		v.local[o.key] = val
		delete(v.deleted, o.key)
	case opReplace:
		v.local[o.key] = o.value
		delete(v.deleted, o.key)
	case opDelete:
		delete(v.local, o.key)
		v.deleted[o.key] = true
	case opUpdate:
		m := v.snapshot()
		before := maps.Clone(m)
		o.fn(m)
		for k, val := range m {
			v.local[k] = val
			delete(v.deleted, k)
		}
		for k := range before {
			if _, ok := m[k]; !ok {
				delete(v.local, k)
				v.deleted[k] = true
			}
		}
	}
	if v.private[o.key] && o.kind != opUpdate {
		return
	}
	v.ops = append(v.ops, o)
}

// setPrivate stores a value only the view sees.
func (s *State) setPrivate(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.view.private == nil {
		s.view.private = map[string]bool{}
	}
	s.view.private[key] = true
	s.view.local[key] = value
}

// takeOps returns the writes recorded by a view.
func (s *State) takeOps() []op {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view.ops
}

// reducerFor returns the reducer the state applies to writes of key.
func (s *State) reducerFor(key string) ReducerFunc {
	if s.view == nil {
		return nil
	}
	if r := s.view.reducers[key]; r != nil {
		return r
	}
	return s.view.base.reducerFor(key)
}

// commit applies the writes of a node to s. reducers are those of the
// workflow committing; check is called for each key set or deleted
// without a reducer and may reject the write. It returns the values of
// the keys that changed and the keys that were removed.
func (s *State) commit(ops []op, reducers map[string]ReducerFunc, check func(key string) error) (delta map[string]any, removed []string, err error) {
	touched := map[string]bool{}
	for _, o := range ops {
		if o.kind == opSet && o.reduce == nil {
			o.reduce = reducers[o.key]
		}
		if (o.kind == opSet && o.reduce == nil) || o.kind == opDelete {
			if err := check(o.key); err != nil {
				return nil, nil, err
			}
		}
		if o.kind == opUpdate {
			before := s.Snapshot()
			s.apply(o)
			after := s.Snapshot()
			for k, v := range after {
				if old, ok := before[k]; !ok || !reflect.DeepEqual(old, v) {
					touched[k] = true
				}
			}
			for k := range before {
				if _, ok := after[k]; !ok {
					touched[k] = true
				}
			}
			continue
		}
		s.apply(o)
		touched[o.key] = true
	}
	delta = map[string]any{}
	for k := range touched {
		if v, ok := s.Get(k); ok {
			delta[k] = v
		} else {
			removed = append(removed, k)
		}
	}
	return delta, removed, nil
}

// apply applies one write to s, recording it if s is a view.
func (s *State) apply(o op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.view; v != nil {
		v.record(s, o)
		return
	}
	if s.values == nil {
		s.values = map[string]any{}
	}
	switch o.kind {
	case opSet:
		if o.reduce != nil {
			old := s.values[o.key]
			s.values[o.key] = o.reduce(old, o.value)
		} else {
			s.values[o.key] = o.value
		}
	case opReplace:
		s.values[o.key] = o.value
	case opDelete:
		delete(s.values, o.key)
	case opUpdate:
		o.fn(s.values)
	}
}

// ReducerFunc merges an update written to a key into the key's current
// value, which is nil if the key has no value.
type ReducerFunc func(current, update any) any

// Append returns a reducer that appends updates to a list. If the current
// value is a slice, an update of the same slice type is concatenated and
// an update assignable to its element type is appended, keeping the slice
// type. Otherwise the result is a []any: a []any update is concatenated,
// any other update is appended as one element, and a current value that is
// not a slice becomes the first element.
func Append() ReducerFunc {
	return func(current, update any) any {
		if current != nil && update != nil {
			cv := reflect.ValueOf(current)
			uv := reflect.ValueOf(update)
			if cv.Kind() == reflect.Slice {
				switch {
				case uv.Type() == cv.Type():
					out := reflect.MakeSlice(cv.Type(), 0, cv.Len()+uv.Len())
					return reflect.AppendSlice(reflect.AppendSlice(out, cv), uv).Interface()
				case uv.Type().AssignableTo(cv.Type().Elem()):
					out := reflect.MakeSlice(cv.Type(), 0, cv.Len()+1)
					out = reflect.AppendSlice(out, cv)
					return reflect.Append(out, uv).Interface()
				}
			}
		}
		out := toList(current)
		if u, ok := update.([]any); ok {
			return append(out, u...)
		}
		return append(out, update)
	}
}

// toList returns v as a new []any.
func toList(v any) []any {
	if v == nil {
		return []any{}
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return []any{v}
	}
	out := make([]any, rv.Len(), rv.Len()+1)
	for i := range rv.Len() {
		out[i] = rv.Index(i).Interface()
	}
	return out
}

// reducerSet returns the reducers that apply to writes to s.
func (s *State) reducerSet() map[string]ReducerFunc {
	if s.view == nil {
		return nil
	}
	out := maps.Clone(s.view.base.reducerSet())
	if out == nil {
		out = map[string]ReducerFunc{}
	}
	maps.Copy(out, s.view.reducers)
	return out
}

// itemWrites returns the writes of a view as stored for a Map item.
func (s *State) itemWrites() []ItemWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.view
	var out []ItemWrite
	for _, o := range v.ops {
		switch o.kind {
		case opSet:
			out = append(out, ItemWrite{Key: o.key, Value: o.value})
		case opDelete:
			out = append(out, ItemWrite{Key: o.key, Deleted: true})
		case opReplace:
			out = append(out, ItemWrite{Key: o.key, Value: o.value, Replace: true})
		case opUpdate:
			for _, k := range slices.Sorted(maps.Keys(o.diff)) {
				if !v.private[k] {
					out = append(out, ItemWrite{Key: k, Value: o.diff[k], Replace: true})
				}
			}
			for _, k := range o.removed {
				if !v.private[k] {
					out = append(out, ItemWrite{Key: k, Deleted: true, Replace: true})
				}
			}
		}
	}
	return out
}
