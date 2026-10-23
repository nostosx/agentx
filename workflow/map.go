package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"
)

// MapOption configures a Map node.
type MapOption func(*mapNode)

// MapConcurrency bounds the number of items running at once. Zero means
// unbounded.
func MapConcurrency(n int) MapOption { return func(m *mapNode) { m.concurrency = n } }

// MapKeys sets the state keys under which each item sees its element and
// its index; they default to "item" and "index".
func MapKeys(item, index string) MapOption {
	return func(m *mapNode) { m.itemKey, m.indexKey = item, index }
}

type mapNode struct {
	key               string
	body              Node
	concurrency       int
	itemKey, indexKey string
}

// Map returns a node that runs body once per element of the list stored
// under key, for example to process documents a previous node produced.
//
// Each item runs on its own view of the state, in which "item" holds the
// element and "index" its position (see MapKeys); those two keys are never
// committed. Items run concurrently, at most MapConcurrency at once, and
// do not see each other's writes. Once every item succeeded, their writes
// are applied in item order, so the result does not depend on timing: to
// collect results, have each item Set a key with a Reducer such as Append,
// which yields the results in item order. Items that set or delete the
// same key without a reducer fail the node with a ConflictError. The
// first failing item cancels the others and fails the node; retries rerun
// only the items that did not finish.
//
// As a node of a graph, Map saves each finished item's writes with the
// run's Checkpointer and captures the list when its execution starts, so a
// resumed run reruns only unfinished items. Items may call Suspend; their
// Pending keys are "node[i]", and the node resumes once all its items have
// values. AgentNode bodies get the item index in their SubRunID. Items of
// a Map nested in another Map's item run without these features.
func Map(key string, body Node, opts ...MapOption) Node {
	m := &mapNode{key: key, body: body, itemKey: "item", indexKey: "index"}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Run implements Node.
func (m *mapNode) Run(ctx context.Context, st *State) error {
	sc := scopeOf(ctx)
	if sc != nil && sc.index >= 0 {
		sc = nil
		ctx = withScope(ctx, nil)
	}
	items, progress, err := m.items(ctx, sc, st)
	if err != nil {
		return err
	}
	results := make([][]ItemWrite, len(items))
	finished := make([]bool, len(items))
	if progress != nil {
		sc.r.mu.Lock()
		for i, ws := range progress.Done {
			if i >= 0 && i < len(items) {
				results[i], finished[i] = ws, true
			}
		}
		sc.r.mu.Unlock()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu        sync.Mutex
		firstErr  error
		suspended bool
		wg        sync.WaitGroup
	)
	var sem chan struct{}
	if m.concurrency > 0 {
		sem = make(chan struct{}, m.concurrency)
	}
	reducers := st.reducerSet()
	for i, item := range items {
		if finished[i] {
			continue
		}
		if sc != nil && !m.runnable(sc, i) {
			mu.Lock()
			suspended = true
			mu.Unlock()
			continue
		}
		wg.Go(func() {
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
			}
			if ctx.Err() != nil {
				return
			}
			writes, susp, err := m.runItem(ctx, sc, st, reducers, i, item)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case susp:
				suspended = true
			case err != nil:
				if firstErr == nil {
					firstErr = fmt.Errorf("item %d: %w", i, err)
					cancel()
				}
			default:
				results[i], finished[i] = writes, true
				if sc != nil {
					sc.r.mu.Lock()
					if p := sc.r.maps[sc.node]; p != nil {
						if p.Done == nil {
							p.Done = map[int][]ItemWrite{}
						}
						p.Done[i] = writes
					}
					sc.r.mu.Unlock()
				}
			}
			if sc != nil && err == nil && !susp {
				if serr := sc.r.save(ctx); serr != nil && firstErr == nil {
					firstErr = serr
					cancel()
				}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if suspended {
		sc.suspended = true
		return ErrSuspended
	}
	return m.merge(sc, st, results)
}

// runnable reports whether item i may run: it has no suspension waiting
// for a value.
func (m *mapNode) runnable(sc *scope, i int) bool {
	key := sc.node + "[" + strconv.Itoa(i) + "]"
	sc.r.mu.Lock()
	defer sc.r.mu.Unlock()
	for _, p := range sc.r.suspends[key] {
		if p.Resume == nil {
			return false
		}
	}
	return true
}

// items returns the elements of the list, captured when the node's
// execution started, and the node's progress.
func (m *mapNode) items(ctx context.Context, sc *scope, st *State) ([]any, *MapProgress, error) {
	if sc != nil {
		sc.r.mu.Lock()
		p := sc.r.maps[sc.node]
		sc.r.mu.Unlock()
		if p != nil {
			if p.values != nil {
				return p.values, p, nil
			}
			out := make([]any, len(p.Items))
			for i, b := range p.Items {
				if err := json.Unmarshal(b, &out[i]); err != nil {
					return nil, nil, fmt.Errorf("workflow: map %s: decode item %d: %w", sc.node, i, err)
				}
			}
			p.values = out
			return out, p, nil
		}
	}
	v, ok := st.Get(m.key)
	if !ok || v == nil {
		v = []any{}
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, nil, fmt.Errorf("workflow: map over %q: value is a %T, not a list", m.key, v)
	}
	items := make([]any, rv.Len())
	for i := range items {
		items[i] = rv.Index(i).Interface()
	}
	if sc == nil {
		return items, nil, nil
	}
	p := &MapProgress{values: items}
	if sc.r.cp != nil {
		p.Items = make([]json.RawMessage, len(items))
		for i, item := range items {
			b, err := json.Marshal(item)
			if err != nil {
				return nil, nil, fmt.Errorf("workflow: map over %q: item %d is not JSON-encodable, which a Checkpointer requires: %w", m.key, i, err)
			}
			p.Items[i] = b
		}
	}
	sc.r.mu.Lock()
	sc.r.maps[sc.node] = p
	sc.r.mu.Unlock()
	return items, p, sc.r.save(ctx)
}

// runItem runs body for one item and returns its writes.
func (m *mapNode) runItem(ctx context.Context, sc *scope, st *State, reducers map[string]ReducerFunc, i int, item any) (writes []ItemWrite, suspended bool, err error) {
	view := newView(st, reducers)
	view.setPrivate(m.itemKey, item)
	view.setPrivate(m.indexKey, i)
	var isc *scope
	if sc != nil {
		isc = &scope{r: sc.r, node: sc.node, index: i, attempt: sc.attempt, try: sc.try, visit: sc.visit}
		ctx = withScope(ctx, isc)
		sc.r.emit(Event{Type: NodeStart, Node: sc.node, Index: i, Attempt: sc.try})
	}
	begin := time.Now()
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("panic: %v", rec)
			}
		}()
		err = m.body.Run(ctx, view)
	}()
	if isc != nil {
		if isc.suspended {
			return nil, true, nil
		}
		ev := Event{Type: NodeEnd, Node: sc.node, Index: i, Attempt: sc.try, Duration: time.Since(begin)}
		if err != nil {
			ev.Type, ev.Err = NodeFailed, err
		}
		sc.r.emit(ev)
	}
	if err != nil {
		return nil, false, err
	}
	writes = view.itemWrites()
	if sc != nil && sc.r.cp != nil {
		for _, w := range writes {
			if _, err := json.Marshal(w.Value); err != nil {
				return nil, false, fmt.Errorf("state value %q is not JSON-encodable, which a Checkpointer requires: %w", w.Key, err)
			}
		}
	}
	return writes, false, nil
}

// merge applies the writes of the items to st in item order.
func (m *mapNode) merge(sc *scope, st *State, results [][]ItemWrite) error {
	name := "map"
	if sc != nil {
		name = sc.node
	}
	owner := map[string]int{}
	for i, ws := range results {
		for _, w := range ws {
			if w.Replace || (!w.Deleted && st.reducerFor(w.Key) != nil) {
				continue
			}
			if j, ok := owner[w.Key]; ok && j != i {
				return &ConflictError{Key: w.Key, Nodes: []string{name + "[" + strconv.Itoa(j) + "]", name + "[" + strconv.Itoa(i) + "]"}}
			}
			owner[w.Key] = i
		}
	}
	for _, ws := range results {
		for _, w := range ws {
			switch {
			case w.Deleted:
				st.apply(op{kind: opDelete, key: w.Key})
			case w.Replace:
				st.apply(op{kind: opReplace, key: w.Key, value: w.Value})
			default:
				st.apply(op{kind: opSet, key: w.Key, value: w.Value})
			}
		}
	}
	return nil
}
