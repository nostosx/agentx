package workflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nostosx/kitx/syncmap"
)

// Start and End are the implicit entry and exit of a graph.
const (
	Start = "__start__"
	End   = "__end__"
)

// BranchFunc chooses the successors to run after a node. It returns names
// among the branch targets; other targets are skipped. Returning End or no
// names skips all targets.
type BranchFunc func(ctx context.Context, st *State) ([]string, error)

type branch struct {
	cond    BranchFunc
	targets []string
}

type linkKind int

const (
	linkEdge linkKind = iota
	linkBranch
	linkFallback
)

// link is an edge of the graph in the order it was added.
type link struct {
	from, to string
	kind     linkKind
}

// Graph builds a workflow. Methods record the first error, which Compile
// returns, so calls can be chained.
type Graph struct {
	nodes    map[string]*nodeSpec
	order    []string
	edges    map[string][]string
	branches map[string][]*branch
	links    []link
	errs     []error
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{
		nodes:    map[string]*nodeSpec{},
		edges:    map[string][]string{},
		branches: map[string][]*branch{},
	}
}

// AddNode adds a named node.
func (g *Graph) AddNode(name string, n Node, opts ...NodeOption) *Graph {
	switch {
	case name == "" || name == Start || name == End:
		g.errs = append(g.errs, fmt.Errorf("workflow: invalid node name %q", name))
	case g.nodes[name] != nil:
		g.errs = append(g.errs, fmt.Errorf("workflow: duplicate node %q", name))
	case n == nil:
		g.errs = append(g.errs, fmt.Errorf("workflow: node %q is nil", name))
	default:
		spec := &nodeSpec{name: name, node: n, retry: RetryPolicy{MaxAttempts: 1, linear: true}}
		for _, opt := range opts {
			opt(spec)
		}
		spec.retry.MaxAttempts = max(spec.retry.MaxAttempts, 1)
		g.nodes[name] = spec
		g.order = append(g.order, name)
		if spec.fallback != "" {
			g.links = append(g.links, link{from: name, to: spec.fallback, kind: linkFallback})
		}
	}
	return g
}

// AddFunc adds a function node.
func (g *Graph) AddFunc(name string, fn func(ctx context.Context, st *State) error, opts ...NodeOption) *Graph {
	return g.AddNode(name, NodeFunc(fn), opts...)
}

// AddEdge runs to after from completes.
func (g *Graph) AddEdge(from, to string) *Graph {
	g.edges[from] = append(g.edges[from], to)
	g.links = append(g.links, link{from: from, to: to})
	return g
}

// AddChain adds edges between consecutive nodes.
func (g *Graph) AddChain(nodes ...string) *Graph {
	for i := range len(nodes) - 1 {
		g.AddEdge(nodes[i], nodes[i+1])
	}
	return g
}

// AddBranch runs the targets chosen by cond after from completes.
//
// A target may be a node from which from can be reached, which makes a
// loop: choosing it routes back. See Compile for which edges loop back and
// Workflow.Run for how loops run.
func (g *Graph) AddBranch(from string, cond BranchFunc, targets ...string) *Graph {
	if cond == nil || len(targets) == 0 {
		g.errs = append(g.errs, fmt.Errorf("workflow: branch from %q needs a condition and targets", from))
		return g
	}
	g.branches[from] = append(g.branches[from], &branch{cond: cond, targets: slices.Clone(targets)})
	for _, t := range targets {
		g.links = append(g.links, link{from: from, to: t, kind: linkBranch})
	}
	return g
}

// AddCondition is a branch that picks one target by key: choose returns
// a key of routes and the matching node runs.
func (g *Graph) AddCondition(from string, choose func(ctx context.Context, st *State) (string, error), routes map[string]string) *Graph {
	routes = maps.Clone(routes)
	targets := slices.Compact(slices.Sorted(maps.Values(routes)))
	return g.AddBranch(from, func(ctx context.Context, st *State) ([]string, error) {
		key, err := choose(ctx, st)
		if err != nil {
			return nil, err
		}
		t, ok := routes[key]
		if !ok {
			return nil, fmt.Errorf("workflow: no route for %q after %s", key, from)
		}
		return []string{t}, nil
	}, targets...)
}

// Compile validates the graph: every edge must join known nodes and every
// node must be reachable from Start.
//
// Edges are walked depth first from Start, in the order they were added;
// an edge into a node on the current path loops back. Only branch edges,
// from AddBranch or AddCondition, may loop back: a cycle closed by AddEdge
// or OnError is an error.
func (g *Graph) Compile(opts ...Option) (*Workflow, error) {
	errs := slices.Clone(g.errs)
	known := func(n string) bool { return n == Start || n == End || g.nodes[n] != nil }

	// Successors in the order the edges were added, and the kind of the
	// first edge between two nodes that may not loop back.
	succ := map[string][]string{}
	static := map[[2]string]bool{}
	for _, l := range g.links {
		if !slices.Contains(succ[l.from], l.to) {
			succ[l.from] = append(succ[l.from], l.to)
		}
		if l.kind != linkBranch {
			static[[2]string{l.from, l.to}] = true
		}
	}
	for _, l := range g.links {
		switch {
		case !known(l.from) || l.from == End:
			errs = append(errs, fmt.Errorf("workflow: edge from unknown node %q", l.from))
		case !known(l.to) || l.to == Start:
			if l.kind == linkFallback {
				errs = append(errs, fmt.Errorf("workflow: node %q falls back to unknown node %q", l.from, l.to))
			} else {
				errs = append(errs, fmt.Errorf("workflow: edge from %q to unknown node %q", l.from, l.to))
			}
		}
	}
	if len(succ[Start]) == 0 {
		errs = append(errs, errors.New("workflow: no edge from Start"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Classify loop edges by depth-first search.
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	back := map[string][]string{}
	var path []string
	var visit func(string) error
	visit = func(n string) error {
		color[n] = grey
		path = append(path, n)
		for _, s := range succ[n] {
			switch color[s] {
			case grey:
				if static[[2]string{n, s}] {
					i := slices.Index(path, s)
					return fmt.Errorf("workflow: cycle %s -> %s; loop back with AddBranch or AddCondition, or use Loop", strings.Join(path[i:], " -> "), s)
				}
				back[n] = append(back[n], s)
			case white:
				if err := visit(s); err != nil {
					return err
				}
			}
		}
		path = path[:len(path)-1]
		color[n] = black
		return nil
	}
	if err := visit(Start); err != nil {
		return nil, err
	}
	var unreachable []string
	for _, n := range g.order {
		if color[n] == white {
			unreachable = append(unreachable, n)
		}
	}
	if len(unreachable) > 0 {
		return nil, fmt.Errorf("workflow: nodes unreachable from Start: %s", strings.Join(unreachable, ", "))
	}

	// The forward graph leaves out the edges that loop back.
	fsucc := map[string][]string{}
	preds := map[string][]string{}
	for from, tos := range succ {
		for _, to := range tos {
			if slices.Contains(back[from], to) {
				continue
			}
			fsucc[from] = append(fsucc[from], to)
			preds[to] = append(preds[to], from)
		}
	}
	for _, ps := range preds {
		slices.Sort(ps)
	}
	reach := map[string]map[string]bool{}
	for _, n := range g.order {
		seen := map[string]bool{}
		stack := slices.Clone(fsucc[n])
		for len(stack) > 0 {
			s := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if s == End || seen[s] {
				continue
			}
			seen[s] = true
			stack = append(stack, fsucc[s]...)
		}
		reach[n] = seen
	}

	edges := make(map[string][]string, len(g.edges))
	for from, tos := range g.edges {
		edges[from] = slices.Clone(tos)
	}
	branches := make(map[string][]*branch, len(g.branches))
	for from, bs := range g.branches {
		for _, b := range bs {
			branches[from] = append(branches[from], &branch{cond: b.cond, targets: slices.Clone(b.targets)})
		}
	}
	nodes := make(map[string]*nodeSpec, len(g.nodes))
	for name, spec := range g.nodes {
		c := *spec
		nodes[name] = &c
	}
	w := &Workflow{
		name:     "workflow",
		nodes:    nodes,
		order:    slices.Clone(g.order),
		edges:    edges,
		branches: branches,
		links:    slices.Clone(g.links),
		succ:     fsucc,
		preds:    preds,
		back:     back,
		reach:    reach,
		held:     &syncmap.Map[string, *heldRun]{},
	}
	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

// MustCompile is like Compile but panics on error.
func (g *Graph) MustCompile(opts ...Option) *Workflow {
	w, err := g.Compile(opts...)
	if err != nil {
		panic(err)
	}
	return w
}

// Mermaid renders the workflow as a Mermaid flowchart. Edges added with
// AddEdge are solid, branch edges dashed, edges that loop back are labeled
// "loop" and OnError fallbacks "on error".
func (w *Workflow) Mermaid() string {
	ids := map[string]string{Start: "start", End: "end"}
	for i, n := range w.order {
		ids[n] = fmt.Sprintf("n%d", i)
	}
	label := func(s string) string {
		return strings.NewReplacer(`"`, "#quot;", "<", "#lt;", ">", "#gt;").Replace(s)
	}
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	b.WriteString("    start([start])\n")
	for _, n := range w.order {
		shape := `["%s"]`
		if _, ok := w.nodes[n].node.(*mapNode); ok {
			shape = `[["%s"]]`
		}
		fmt.Fprintf(&b, "    %s"+shape+"\n", ids[n], label(n))
	}
	b.WriteString("    end([end])\n")
	seen := map[link]bool{}
	for _, l := range w.links {
		if seen[l] {
			continue
		}
		seen[l] = true
		from, to := ids[l.from], ids[l.to]
		switch {
		case l.kind == linkFallback:
			fmt.Fprintf(&b, "    %s -. on error .-> %s\n", from, to)
		case l.kind == linkBranch && slices.Contains(w.back[l.from], l.to):
			fmt.Fprintf(&b, "    %s -. loop .-> %s\n", from, to)
		case l.kind == linkBranch:
			fmt.Fprintf(&b, "    %s -.-> %s\n", from, to)
		default:
			fmt.Fprintf(&b, "    %s --> %s\n", from, to)
		}
	}
	return b.String()
}
