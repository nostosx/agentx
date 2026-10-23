package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/nostosx/kitx/syncmap"

	"github.com/nostosx/agentx/loop"
)

// Workflow is a compiled graph. It is safe for concurrent runs.
type Workflow struct {
	name     string
	nodes    map[string]*nodeSpec
	order    []string
	edges    map[string][]string
	branches map[string][]*branch
	links    []link
	// succ and preds form the forward graph, without the edges in back,
	// which loop back; reach holds the nodes forward-reachable from each
	// node.
	succ  map[string][]string
	preds map[string][]string
	back  map[string][]string
	reach map[string]map[string]bool

	listeners      []func(Event)
	maxConcurrency int
	maxSteps       int
	checkpointer   Checkpointer
	reducers       map[string]ReducerFunc
	// held keeps suspended runs of workflows without a Checkpointer, so
	// they can be resumed in this process.
	held *syncmap.Map[string, *heldRun]
	// parent receives the events of a run nested with AsNode.
	parent func(Event)
}

type heldRun struct {
	cp *Checkpoint
	st *State
}

// NodeError reports the failure of a node.
type NodeError struct {
	Node string
	Err  error
}

func (e *NodeError) Error() string { return fmt.Sprintf("workflow node %s: %v", e.Node, e.Err) }

func (e *NodeError) Unwrap() error { return e.Err }

// errConsumerStopped ends a run whose consumer stopped iterating.
var errConsumerStopped = errors.New("workflow: consumer stopped iterating")

type nodeStatus int

const (
	statusPending nodeStatus = iota
	statusRunning
	statusDone
	statusSkipped
	statusFailed
	statusSuspended
)

// run is the state of one run.
type run struct {
	w  *Workflow
	id string
	cp Checkpointer
	st *State
	// nested is set for a run nested with AsNode, whose state is the
	// parent node's view.
	nested bool

	mu        sync.Mutex
	status    map[string]nodeStatus
	routes    map[string][]string
	activated map[string]bool
	attempts  map[string]int
	tries     map[string]int
	visits    map[string]int
	// same marks nodes whose interrupted attempt runs again on resume,
	// without counting a new attempt or step.
	same     map[string]bool
	steps    int
	writers  map[string][]string
	suspends map[string][]*SuspendPoint
	maps     map[string]*MapProgress
	writes   map[string]ItemWrite
	restarts []string
	errMsg   string
	runState Status

	saveMu sync.Mutex
	qmu    sync.Mutex
	queue  []Event
	notify chan struct{}
}

type outcome struct {
	node      string
	err       error
	fatal     bool
	suspended bool
	view      *State
}

func newRun(w *Workflow, id string, st *State) *run {
	return &run{
		w: w, id: id, cp: w.checkpointer, st: st,
		status:    map[string]nodeStatus{},
		routes:    map[string][]string{},
		activated: map[string]bool{},
		attempts:  map[string]int{},
		tries:     map[string]int{},
		visits:    map[string]int{},
		same:      map[string]bool{},
		writers:   map[string][]string{},
		suspends:  map[string][]*SuspendPoint{},
		maps:      map[string]*MapProgress{},
		writes:    map[string]ItemWrite{},
		runState:  StatusRunning,
		notify:    make(chan struct{}, 1),
	}
}

// Run executes the workflow on st, which may be nil, and returns the
// state. It consumes Stream; see there for the semantics. A run that
// suspends returns a *Suspension error, which matches ErrSuspended.
func (w *Workflow) Run(ctx context.Context, st *State, opts ...RunOption) (*State, error) {
	if st == nil {
		st = NewState(nil)
	}
	return collect(w.Stream(ctx, st, opts...), st)
}

// Resume continues a run from its checkpoint, after a crash, a failure or
// a suspension, and returns the state; see ResumeStream.
func (w *Workflow) Resume(ctx context.Context, runID string, resume map[string]any, opts ...RunOption) (*State, error) {
	var st *State
	return collect(w.resumeStream(ctx, runID, resume, nil, opts, &st), nil, &st)
}

// collect drains a run's events and returns its state and error.
func collect(events iter.Seq2[*Event, error], st *State, stp ...**State) (*State, error) {
	var end *Event
	for ev, err := range events {
		if err != nil {
			if len(stp) > 0 {
				st = *stp[0]
			}
			return st, err
		}
		if ev.Type == RunEnd {
			end = ev
		}
	}
	if len(stp) > 0 {
		st = *stp[0]
	}
	if end != nil && end.Status == StatusSuspended {
		return st, &Suspension{RunID: end.RunID, Pending: end.Suspended}
	}
	return st, nil
}

// Stream executes the workflow on st, which may be nil, and returns its
// events. The last event is RunEnd; a failed run then yields its error.
//
// A node runs once all its predecessors have finished or been skipped and
// at least one incoming edge was taken; otherwise it is skipped. Edges
// that loop back do not count as predecessors. The first failure, after
// retries and fallbacks, cancels the remaining nodes and fails the run
// once they returned.
//
// When a node's branch routes back to a loop target, the run waits until
// no node reachable from the target is running or suspended, resets those
// nodes to pending and runs the target again; they run again as they are
// reached. A node either loops back or continues: choosing a loop target
// together with other targets is an error. Every node execution counts as
// a step; see MaxSteps.
//
// With a Checkpointer the run is saved when a node attempt starts and
// after each node, so Resume continues it after a crash. Node code runs at
// least once: an attempt cut off by a crash runs again, with the same
// attempt number. State writes take effect exactly once, because they are
// committed with the node's completion. If the consumer stops iterating,
// running nodes are cancelled and the run can be resumed.
func (w *Workflow) Stream(ctx context.Context, st *State, opts ...RunOption) iter.Seq2[*Event, error] {
	return func(yield func(*Event, error) bool) {
		if st == nil {
			st = NewState(nil)
		}
		c := w.configure(opts)
		id := c.runID
		if id == "" {
			id = "wf_" + uuid.NewV7().String()
		}
		if !runIDPattern.MatchString(id) {
			yield(nil, fmt.Errorf("workflow: invalid run ID %q", id))
			return
		}
		if c.runID != "" && c.w.exists(ctx, id) {
			yield(nil, fmt.Errorf("workflow: run %s already exists; resume it or discard it", id))
			return
		}
		r := newRun(c.w, id, st)
		r.nested = st.view != nil && c.w.parent != nil
		r.exec(ctx, yield, func(ctx context.Context) error {
			chosen, err := r.route(ctx, Start, st)
			if err != nil {
				return err
			}
			r.status[Start] = statusDone
			r.routes[Start] = chosen
			return nil
		})
	}
}

// ResumeStream is like Resume but returns the events of the resumed run;
// see Stream.
//
// resume maps the keys of pending suspensions (see Pending) to their
// values, which are encoded as JSON unless they are a json.RawMessage. A
// node runs again once all its suspensions have values; keys left out stay
// pending. Nodes that were running when the run stopped run again with
// the same attempt number; failed nodes run again with fresh retries.
//
// Without a Checkpointer, only runs that suspended in this process can be
// resumed: the Workflow keeps them in memory until they complete or are
// discarded.
func (w *Workflow) ResumeStream(ctx context.Context, runID string, resume map[string]any, opts ...RunOption) iter.Seq2[*Event, error] {
	var st *State
	return w.resumeStream(ctx, runID, resume, nil, opts, &st)
}

func (w *Workflow) resumeStream(ctx context.Context, runID string, resume map[string]any, base *State, opts []RunOption, stp **State) iter.Seq2[*Event, error] {
	return func(yield func(*Event, error) bool) {
		c := w.configure(opts)
		cp, held, err := c.w.load(ctx, runID)
		if err != nil {
			yield(nil, err)
			return
		}
		if cp.Version > CheckpointVersion {
			yield(nil, fmt.Errorf("workflow: run %s has checkpoint version %d; this version supports up to %d", runID, cp.Version, CheckpointVersion))
			return
		}
		st := base
		switch {
		case st != nil:
			replayWrites(st, cp.Writes)
		case held != nil:
			st = held
		default:
			values, err := decodeValues(cp.Values)
			if err != nil {
				yield(nil, err)
				return
			}
			st = NewState(values)
		}
		*stp = st
		r := newRun(c.w, runID, st)
		r.nested = base != nil
		if err := r.restore(cp, resume); err != nil {
			yield(nil, err)
			return
		}
		r.exec(ctx, yield, func(ctx context.Context) error { return r.save(ctx) })
	}
}

// Discard deletes the checkpoint of a run that will not be resumed, or
// the copy kept in memory without a Checkpointer. A missing run is not an
// error.
func (w *Workflow) Discard(ctx context.Context, runID string, opts ...RunOption) error {
	c := w.configure(opts)
	c.w.held.Delete(runID)
	if c.w.checkpointer == nil {
		return nil
	}
	if err := c.w.checkpointer.Delete(ctx, runID); err != nil && !errors.Is(err, ErrCheckpointNotFound) {
		return err
	}
	return nil
}

func (w *Workflow) configure(opts []RunOption) *runConfig {
	c := &runConfig{w: w.clone()}
	for _, opt := range opts {
		opt.applyRun(c)
	}
	if c.w.held == nil {
		c.w.held = &syncmap.Map[string, *heldRun]{}
	}
	if c.w.maxSteps == 0 && len(c.w.back) > 0 {
		c.w.maxSteps = DefaultMaxSteps
	}
	return c
}

// exists reports whether a run with the ID is saved.
func (w *Workflow) exists(ctx context.Context, id string) bool {
	_, _, err := w.load(ctx, id)
	return err == nil
}

// load returns the checkpoint of a run and, for a run kept in memory, its
// state.
func (w *Workflow) load(ctx context.Context, id string) (*Checkpoint, *State, error) {
	if h, ok := w.held.Get(id); ok {
		return h.cp.clone(), h.st, nil
	}
	if w.checkpointer == nil {
		return nil, nil, fmt.Errorf("workflow: run %s: %w", id, ErrCheckpointNotFound)
	}
	cp, err := w.checkpointer.Load(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("workflow: run %s: %w", id, err)
	}
	return cp, nil, nil
}

// replayWrites applies the writes of a nested run to the parent's view.
func replayWrites(st *State, writes map[string]ItemWrite) {
	for _, k := range slices.Sorted(maps.Keys(writes)) {
		wr := writes[k]
		if wr.Deleted {
			st.apply(op{kind: opDelete, key: k})
		} else {
			st.apply(op{kind: opReplace, key: k, value: wr.Value})
		}
	}
}

// restore loads the bookkeeping of cp and applies resume values.
func (r *run) restore(cp *Checkpoint, resume map[string]any) error {
	set := func(nodes []string, s nodeStatus) {
		for _, n := range nodes {
			r.status[n] = s
		}
	}
	set(cp.Done, statusDone)
	set(cp.Skipped, statusSkipped)
	set(cp.Running, statusRunning)
	set(cp.Failed, statusFailed)
	set(cp.Suspended, statusSuspended)
	r.status[Start] = statusDone
	maps.Copy(r.routes, cp.Routes)
	for _, n := range cp.Activated {
		r.activated[n] = true
	}
	maps.Copy(r.attempts, cp.Attempts)
	maps.Copy(r.tries, cp.Tries)
	maps.Copy(r.visits, cp.Visits)
	maps.Copy(r.writers, cp.Writers)
	maps.Copy(r.suspends, cp.Suspends)
	maps.Copy(r.maps, cp.Maps)
	maps.Copy(r.writes, cp.Writes)
	r.steps = cp.Steps
	for from, tos := range cp.Routes {
		if r.status[from] != statusDone {
			continue
		}
		for _, to := range tos {
			if slices.Contains(r.w.back[from], to) && !slices.Contains(r.restarts, to) {
				r.restarts = append(r.restarts, to)
			}
		}
	}
	for key, v := range resume {
		points := r.suspends[key]
		i := slices.IndexFunc(points, func(p *SuspendPoint) bool { return p.Resume == nil })
		if i < 0 {
			return fmt.Errorf("workflow: run %s has no pending suspension %q", r.id, key)
		}
		b, ok := v.(json.RawMessage)
		if !ok {
			var err error
			if b, err = json.Marshal(v); err != nil {
				return fmt.Errorf("workflow: resume value for %q: %w", key, err)
			}
		}
		points[i].Resume = b
	}
	for _, n := range r.w.order {
		switch r.status[n] {
		case statusRunning:
			r.status[n] = statusPending
			r.same[n] = true
		case statusFailed:
			r.status[n] = statusPending
			delete(r.tries, n)
		case statusSuspended:
			if !r.unresolved(n) {
				r.status[n] = statusPending
				r.same[n] = true
			}
		}
	}
	return nil
}

// keysOf returns the suspension keys of node: its own and its items'.
func (r *run) keysOf(node string) []string {
	var keys []string
	for k := range r.suspends {
		if k == node || strings.HasPrefix(k, node+"[") && strings.HasSuffix(k, "]") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// unresolved reports whether node has suspensions without a value. The
// caller holds r.mu or owns r.
func (r *run) unresolved(node string) bool {
	for _, k := range r.keysOf(node) {
		for _, p := range r.suspends[k] {
			if p.Resume == nil {
				return true
			}
		}
	}
	return false
}

// pending returns the suspensions waiting for values.
func (r *run) pending() []*Pending {
	var out []*Pending
	for _, n := range r.w.order {
		if r.status[n] != statusSuspended {
			continue
		}
		keys := r.keysOf(n)
		slices.SortStableFunc(keys, func(a, b string) int { return indexOf(a) - indexOf(b) })
		for _, k := range keys {
			for _, p := range r.suspends[k] {
				if p.Resume != nil {
					continue
				}
				out = append(out, newPending(k, n, p))
			}
		}
	}
	return out
}

func indexOf(key string) int {
	_, index, ok := strings.CutLast(key, "[")
	if !ok {
		return -1
	}
	var n int
	if _, err := fmt.Sscanf(index, "%d]", &n); err != nil {
		return -1
	}
	return n
}

func newPending(key, node string, p *SuspendPoint) *Pending {
	pd := &Pending{Key: key, Node: node, Index: -1, Payload: p.Payload}
	if key != node {
		pd.Index = indexOf(key)
	}
	if p.Agent {
		var intr loop.Interrupt
		if json.Unmarshal(p.Payload, &intr) == nil {
			pd.Interrupt = &intr
		}
	}
	return pd
}

// exec runs the scheduler: begin initializes the run, then nodes run until
// none can.
func (r *run) exec(ctx context.Context, yield func(*Event, error) bool, begin func(context.Context) error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstErr error
	stopped := false
	deliver := func() {
		for _, ev := range r.drain() {
			if lifecycle(ev.Type) {
				for _, l := range r.w.listeners {
					l(ev)
				}
			}
			if stopped {
				continue
			}
			if !yield(&ev, nil) {
				stopped = true
				cancel()
				if firstErr == nil {
					firstErr = errConsumerStopped
				}
			}
		}
	}
	r.emit(Event{Type: RunStart})
	deliver()
	if stopped {
		return
	}
	if err := begin(ctx); err != nil {
		firstErr = err
	}
	results := make(chan outcome, len(r.w.nodes))
	running := 0
	var sem chan struct{}
	if r.w.maxConcurrency > 0 {
		sem = make(chan struct{}, r.w.maxConcurrency)
	}
	start := func(name string) {
		running++
		spec := r.w.nodes[name]
		go func() {
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					results <- outcome{node: name, err: ctx.Err()}
					return
				}
			}
			results <- r.runNode(ctx, spec)
		}()
	}
	for {
		if firstErr == nil {
			if err := r.schedule(ctx, start); err != nil {
				firstErr = err
				cancel()
			}
		}
		deliver()
		if running == 0 {
			break
		}
		select {
		case out := <-results:
			running--
			if err := r.finish(ctx, out, firstErr != nil); err != nil && firstErr == nil {
				firstErr = err
				cancel()
			}
		case <-r.notify:
		}
	}
	firstErr = r.end(ctx, firstErr)
	deliver()
	if firstErr != nil && !stopped {
		yield(nil, firstErr)
	}
}

// lifecycle reports whether OnEvent listeners receive events of type t.
func lifecycle(t EventType) bool {
	switch t {
	case NodeStart, NodeEnd, NodeSkipped, NodeFailed, NodeSuspended:
		return true
	}
	return false
}

// end records how the run ended and returns execution and persistence errors.
func (r *run) end(ctx context.Context, err error) error {
	r.mu.Lock()
	pending := r.pending()
	switch {
	case err != nil:
		r.runState, r.errMsg = StatusFailed, err.Error()
	case len(pending) > 0:
		r.runState = StatusSuspended
	default:
		r.runState = StatusCompleted
	}
	status := r.runState
	r.mu.Unlock()
	switch status {
	case StatusCompleted:
		r.w.held.Delete(r.id)
		if r.cp != nil {
			if derr := r.cp.Delete(context.WithoutCancel(ctx), r.id); derr != nil && !errors.Is(derr, ErrCheckpointNotFound) {
				err = errors.Join(err, fmt.Errorf("workflow: delete checkpoint: %w", derr))
			}
		}
	case StatusSuspended:
		if r.cp == nil {
			r.mu.Lock()
			r.w.held.Set(r.id, &heldRun{cp: r.checkpoint(nil), st: r.st})
			r.mu.Unlock()
		} else if serr := r.save(ctx); serr != nil {
			err = errors.Join(err, serr)
		}
	default:
		if r.cp == nil {
			r.w.held.Delete(r.id)
		} else if serr := r.save(ctx); serr != nil {
			err = errors.Join(err, serr)
		}
	}
	if err != nil {
		status = StatusFailed
		r.mu.Lock()
		r.runState, r.errMsg = status, err.Error()
		r.mu.Unlock()
	}
	ev := Event{Type: RunEnd, Status: status, Err: err}
	if status == StatusSuspended {
		ev.Suspended = pending
	}
	r.emit(ev)
	return err
}

// schedule starts the nodes that are ready and skips those that cannot
// run, until no more can.
func (r *run) schedule(ctx context.Context, start func(string)) error {
	for changed := true; changed; {
		changed = false
		r.mu.Lock()
		if r.applyRestarts() {
			changed = true
		}
		frozen := r.frozen()
		var ready []string
		var limit error
		for _, n := range r.w.order {
			if r.status[n] != statusPending || frozen[n] {
				continue
			}
			resolved, taken := r.inputs(n)
			if !resolved {
				continue
			}
			if !taken {
				r.status[n] = statusSkipped
				r.emitLocked(Event{Type: NodeSkipped, Node: n, Index: -1})
				changed = true
				continue
			}
			if !r.same[n] {
				if r.w.maxSteps > 0 && r.steps >= r.w.maxSteps {
					limit = &NodeError{Node: n, Err: fmt.Errorf("%w: %d node executions", ErrStepLimit, r.w.maxSteps)}
					break
				}
				r.steps++
				r.visits[n]++
				r.tries[n] = 0
			}
			r.status[n] = statusRunning
			ready = append(ready, n)
		}
		r.mu.Unlock()
		if len(ready) > 0 {
			if err := r.save(ctx); err != nil {
				return err
			}
			for _, n := range ready {
				start(n)
			}
		}
		if limit != nil {
			return limit
		}
	}
	return nil
}

// inputs reports whether all forward predecessors of n are resolved and
// whether one of them, or a loop, activated it. The caller holds r.mu.
func (r *run) inputs(n string) (resolved, taken bool) {
	taken = r.activated[n]
	for _, p := range r.w.preds[n] {
		switch r.status[p] {
		case statusDone:
			if slices.Contains(r.routes[p], n) {
				taken = true
			}
		case statusSkipped:
		default:
			return false, false
		}
	}
	return true, taken
}

// region returns the loop target and the nodes reachable from it.
func (r *run) region(target string) map[string]bool {
	reg := maps.Clone(r.w.reach[target])
	reg[target] = true
	return reg
}

// frozen returns the nodes of pending loop restarts. The caller holds
// r.mu.
func (r *run) frozen() map[string]bool {
	out := map[string]bool{}
	for _, t := range r.restarts {
		maps.Copy(out, r.region(t))
	}
	return out
}

// applyRestarts resets the regions of pending loop restarts in which no
// node is running or suspended. The caller holds r.mu.
func (r *run) applyRestarts() bool {
	applied := false
	for i := 0; i < len(r.restarts); {
		target := r.restarts[i]
		reg := r.region(target)
		busy := false
		for n := range reg {
			if s := r.status[n]; s == statusRunning || s == statusSuspended {
				busy = true
				break
			}
		}
		if busy {
			i++
			continue
		}
		r.restarts = slices.Delete(r.restarts, i, i+1)
		for n := range reg {
			delete(r.status, n)
			delete(r.routes, n)
			delete(r.activated, n)
			delete(r.same, n)
			delete(r.tries, n)
			delete(r.maps, n)
			for _, k := range r.keysOf(n) {
				delete(r.suspends, k)
			}
		}
		for k, ws := range r.writers {
			ws = slices.DeleteFunc(slices.Clone(ws), func(n string) bool { return reg[n] })
			if len(ws) == 0 {
				delete(r.writers, k)
			} else {
				r.writers[k] = ws
			}
		}
		r.activated[target] = true
		applied = true
	}
	return applied
}

// route evaluates the branches of a finished node on st and returns the
// successors it takes.
func (r *run) route(ctx context.Context, name string, st *State) ([]string, error) {
	taken := slices.Clone(r.w.edges[name])
	for _, b := range r.w.branches[name] {
		chosen, err := callBranch(ctx, b.cond, st)
		if err != nil {
			return nil, &NodeError{Node: name, Err: fmt.Errorf("branch: %w", err)}
		}
		for _, c := range chosen {
			if c == End {
				continue
			}
			if !slices.Contains(b.targets, c) {
				return nil, &NodeError{Node: name, Err: fmt.Errorf("branch chose %q, which is not a target", c)}
			}
			if !slices.Contains(taken, c) {
				taken = append(taken, c)
			}
		}
	}
	loops := 0
	for _, t := range taken {
		if slices.Contains(r.w.back[name], t) {
			loops++
		}
	}
	if loops > 0 && loops < len(taken) {
		return nil, &NodeError{Node: name, Err: fmt.Errorf("routes back to a loop target and continues at once: %s", strings.Join(taken, ", "))}
	}
	return taken, nil
}

// callBranch runs a branch condition, converting a panic into an error so a
// faulty condition fails the run instead of crashing it.
func callBranch(ctx context.Context, cond BranchFunc, st *State) (chosen []string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			chosen, err = nil, fmt.Errorf("panic: %v", rec)
		}
	}()
	return cond(ctx, st)
}

// finish records the outcome of a node. failing is set when the run is
// already failing; finished nodes are still committed so that resuming
// does not repeat them.
func (r *run) finish(ctx context.Context, out outcome, failing bool) error {
	name := out.node
	spec := r.w.nodes[name]
	if out.suspended {
		r.mu.Lock()
		r.status[name] = statusSuspended
		for _, k := range r.keysOf(name) {
			for _, p := range r.suspends[k] {
				if p.Resume == nil {
					r.emitLocked(Event{Type: NodeSuspended, Node: name, Index: indexOf(k), Pending: newPending(k, name, p)})
				}
			}
		}
		r.mu.Unlock()
		return r.save(ctx)
	}
	fail := func(err error) error {
		r.mu.Lock()
		if ctx.Err() != nil && out.err != nil && !out.fatal {
			// Cancelled with the run: it runs again, with the same attempt,
			// when the run is resumed, as after a crash.
			r.status[name] = statusRunning
		} else {
			r.status[name] = statusFailed
		}
		r.mu.Unlock()
		_ = r.save(ctx)
		if _, ok := errors.AsType[*NodeError](err); ok {
			return err
		}
		return &NodeError{Node: name, Err: err}
	}
	var (
		ops    []op
		routes []string
	)
	switch {
	case out.err != nil && (out.fatal || failing || spec.fallback == "" || ctx.Err() != nil):
		return fail(out.err)
	case out.err != nil:
		ops = []op{{kind: opSet, key: ErrorKey(name), value: out.err.Error()}}
		routes = []string{spec.fallback}
	default:
		ops = out.view.takeOps()
		r.mu.Lock()
		frozen := r.frozen()[name]
		r.mu.Unlock()
		if !frozen {
			var err error
			if routes, err = r.route(context.WithoutCancel(ctx), name, out.view); err != nil {
				return fail(err)
			}
		}
	}
	// Commit and mark the node done atomically, so a checkpoint never
	// holds the writes of a node without its completion or the reverse.
	r.mu.Lock()
	delta, removed, err := r.commit(name, ops)
	if err != nil {
		r.mu.Unlock()
		return fail(err)
	}
	if len(delta) > 0 || len(removed) > 0 {
		slices.Sort(removed)
		r.emitLocked(Event{Type: StateUpdated, Node: name, Index: -1, Delta: delta, Removed: removed})
	}
	r.status[name] = statusDone
	r.routes[name] = routes
	delete(r.same, name)
	delete(r.maps, name)
	for _, k := range r.keysOf(name) {
		delete(r.suspends, k)
	}
	for _, t := range routes {
		if slices.Contains(r.w.back[name], t) && !slices.Contains(r.restarts, t) {
			r.restarts = append(r.restarts, t)
		}
	}
	r.mu.Unlock()
	return r.save(ctx)
}

// commit applies the writes of node to the run's state, detecting
// conflicts with nodes the graph does not order. The caller holds r.mu.
func (r *run) commit(node string, ops []op) (map[string]any, []string, error) {
	check := func(key string) error {
		for _, w := range r.writers[key] {
			if w != node && !r.w.reach[w][node] && !r.w.reach[node][w] {
				nodes := []string{w, node}
				slices.Sort(nodes)
				return &ConflictError{Key: key, Nodes: nodes}
			}
		}
		if !slices.Contains(r.writers[key], node) {
			r.writers[key] = append(r.writers[key], node)
		}
		return nil
	}
	// Check every write before applying any, so a conflict leaves the
	// state unchanged.
	for _, o := range ops {
		if (o.kind == opSet && o.reduce == nil && r.w.reducers[o.key] == nil && r.st.reducerFor(o.key) == nil) || o.kind == opDelete {
			if err := check(o.key); err != nil {
				return nil, nil, err
			}
		}
	}
	delta, removed, err := r.st.commit(ops, r.w.reducers, func(string) error { return nil })
	if err != nil {
		return nil, nil, err
	}
	if r.nested {
		for k, v := range delta {
			r.writes[k] = ItemWrite{Key: k, Value: v}
		}
		for _, k := range removed {
			r.writes[k] = ItemWrite{Key: k, Deleted: true}
		}
	}
	return delta, removed, nil
}

// runNode runs the attempts of a node and returns its outcome.
func (r *run) runNode(ctx context.Context, spec *nodeSpec) outcome {
	name := spec.name
	var last error
	for {
		r.mu.Lock()
		resumed := r.same[name]
		try := r.tries[name]
		r.mu.Unlock()
		if try > 0 && !resumed {
			if err := wait(ctx, spec.retry.delay(try, last)); err != nil {
				return outcome{node: name, err: err}
			}
		}
		if err := ctx.Err(); err != nil {
			return outcome{node: name, err: err}
		}
		sc, err := r.beginAttempt(ctx, name)
		if err != nil {
			return outcome{node: name, err: err, fatal: true}
		}
		r.emit(Event{Type: NodeStart, Node: name, Index: -1, Attempt: sc.try})
		begin := time.Now()
		view, err := r.attempt(ctx, spec, sc)
		ev := Event{Type: NodeEnd, Node: name, Index: -1, Attempt: sc.try, Duration: time.Since(begin)}
		if sc.suspended {
			return outcome{node: name, suspended: true}
		}
		if err != nil {
			ev.Type, ev.Err = NodeFailed, err
		}
		r.emit(ev)
		if err == nil {
			return outcome{node: name, view: view}
		}
		if errors.Is(err, context.Canceled) || ctx.Err() != nil || !spec.retry.retryable(sc.try, err) {
			return outcome{node: name, err: err}
		}
		last = err
	}
}

// beginAttempt counts a new attempt of name, or takes over the interrupted
// one on resume, and saves the run.
func (r *run) beginAttempt(ctx context.Context, name string) (*scope, error) {
	r.mu.Lock()
	if r.same[name] && r.tries[name] > 0 {
		delete(r.same, name)
	} else {
		delete(r.same, name)
		r.tries[name]++
		r.attempts[name]++
	}
	sc := &scope{r: r, node: name, index: -1, attempt: r.attempts[name], try: r.tries[name], visit: r.visits[name]}
	r.mu.Unlock()
	return sc, r.save(ctx)
}

var errAttemptTimeout = errors.New("attempt timeout")

// attempt runs one attempt of a node on a fresh view of the state.
func (r *run) attempt(ctx context.Context, spec *nodeSpec, sc *scope) (view *State, err error) {
	view = newView(r.st, r.w.reducers)
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	actx := withScope(ctx, sc)
	if spec.timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeoutCause(actx, spec.timeout, errAttemptTimeout)
		defer func() {
			if err != nil && ctx.Err() == nil && errors.Is(context.Cause(actx), errAttemptTimeout) {
				err = fmt.Errorf("%w: %w", ErrNodeTimeout, err)
			}
			cancel()
		}()
	}
	return view, spec.node.Run(actx, view)
}

// checkpoint returns the persisted form of the run. values are the encoded
// state values, or nil for a run kept in memory. The caller holds r.mu.
func (r *run) checkpoint(values map[string]json.RawMessage) *Checkpoint {
	cp := &Checkpoint{
		Version:   CheckpointVersion,
		RunID:     r.id,
		Workflow:  r.w.name,
		Status:    r.runState,
		Error:     r.errMsg,
		Values:    values,
		Routes:    map[string][]string{},
		Attempts:  maps.Clone(r.attempts),
		Tries:     maps.Clone(r.tries),
		Visits:    maps.Clone(r.visits),
		Steps:     r.steps,
		Writers:   map[string][]string{},
		Suspends:  map[string][]*SuspendPoint{},
		Maps:      map[string]*MapProgress{},
		UpdatedAt: time.Now().UTC(),
	}
	for _, n := range r.w.order {
		switch r.status[n] {
		case statusDone:
			cp.Done = append(cp.Done, n)
		case statusSkipped:
			cp.Skipped = append(cp.Skipped, n)
		case statusRunning:
			cp.Running = append(cp.Running, n)
		case statusFailed:
			cp.Failed = append(cp.Failed, n)
		case statusSuspended:
			cp.Suspended = append(cp.Suspended, n)
		}
		if r.activated[n] {
			cp.Activated = append(cp.Activated, n)
		}
	}
	for k, v := range r.routes {
		cp.Routes[k] = slices.Clone(v)
	}
	for k, v := range r.writers {
		cp.Writers[k] = slices.Clone(v)
	}
	for k, ps := range r.suspends {
		for _, p := range ps {
			c := *p
			cp.Suspends[k] = append(cp.Suspends[k], &c)
		}
	}
	for k, m := range r.maps {
		cp.Maps[k] = &MapProgress{Items: slices.Clone(m.Items), Done: maps.Clone(m.Done), values: m.values}
	}
	if len(r.writes) > 0 {
		cp.Writes = maps.Clone(r.writes)
	}
	return cp
}

// save persists the run if it has a Checkpointer. It uses a context
// without cancellation, so a cancelled run is still saved.
func (r *run) save(ctx context.Context) error {
	if r.cp == nil {
		return nil
	}
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	// Values and bookkeeping are read together: commits hold r.mu.
	r.mu.Lock()
	values, err := encodeValues(r.st.Snapshot())
	if err != nil {
		r.mu.Unlock()
		return err
	}
	cp := r.checkpoint(values)
	r.mu.Unlock()
	if err := r.cp.Save(context.WithoutCancel(ctx), cp); err != nil {
		return fmt.Errorf("workflow: save checkpoint: %w", err)
	}
	return nil
}

// emit queues an event for the consumer of the run.
func (r *run) emit(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitLocked(ev)
}

// emitLocked is emit for callers holding r.mu.
func (r *run) emitLocked(ev Event) {
	ev.Workflow, ev.RunID, ev.Step = r.w.name, r.id, r.steps
	if ev.Type == RunStart || ev.Type == RunEnd {
		ev.Index = -1
	}
	if r.w.parent != nil && ev.Type != RunStart && ev.Type != RunEnd {
		r.w.parent(ev)
	}
	r.qmu.Lock()
	r.queue = append(r.queue, ev)
	r.qmu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// drain returns the queued events.
func (r *run) drain() []Event {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	q := r.queue
	r.queue = nil
	return q
}

// AsNode returns the workflow as a node of another workflow, sharing the
// parent's state; the nested run's writes are committed with the node.
// Its events are forwarded to the parent run with Node set to
// "parent/child".
//
// Inside a workflow run, the nested run has the ID SubRunID of the node's
// attempt and uses the parent's Checkpointer, so a parent resumed after a
// crash resumes the nested run where it stopped. When the nested run
// suspends, the node suspends with the nested run's []*Pending as payload;
// resume the node's key with a map from the nested keys to their values.
func (w *Workflow) AsNode() Node {
	return NodeFunc(func(ctx context.Context, st *State) error {
		sc := scopeOf(ctx)
		if sc == nil {
			_, err := w.Run(ctx, st)
			return err
		}
		id := SubRunID(sc.r.id, sc.node, sc.attempt, sc.index)
		parent, node := sc.r, sc.node
		opts := []RunOption{
			WithRunID(id),
			Option(func(c *Workflow) {
				c.checkpointer = parent.cp
				c.parent = func(ev Event) {
					ev.Node = node + "/" + ev.Node
					ev.Workflow, ev.RunID = parent.w.name, parent.id
					parent.emit(ev)
				}
			}),
		}
		var resume map[string]any
		if pt, ok := sc.lastResumed(); ok {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(pt.Resume, &raw); err != nil {
				return fmt.Errorf("workflow: resume value for nested workflow %s must map its pending keys to values: %w", node, err)
			}
			resume = map[string]any{}
			for k, v := range raw {
				resume[k] = v
			}
		}
		c := w.configure(opts)
		var err error
		if c.w.exists(ctx, id) {
			var out *State
			_, err = collect(w.resumeStream(ctx, id, resume, st, opts, &out), nil, &out)
		} else {
			_, err = w.Run(ctx, st, opts...)
		}
		if s, ok := errors.AsType[*Suspension](err); ok {
			b, merr := json.Marshal(s.Pending)
			if merr != nil {
				return merr
			}
			_, err = sc.suspend(b, false)
		}
		return err
	})
}
