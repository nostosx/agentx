package builtin

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/nostosx/agentx/tool"
)

// Todo statuses.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// Todo is an item of a TodoList.
type Todo struct {
	Content string `json:"content" jsonschema:"What needs to be done, in imperative form."`
	Status  string `json:"status" jsonschema:"enum=pending,enum=in_progress,enum=completed,description=Current status of the item."`
}

// TodoList is the task list maintained by the todo_write tool. The zero
// value is an empty list. It is safe for concurrent use, so applications can
// display it while an agent runs.
type TodoList struct {
	mu    sync.Mutex
	items []Todo
}

// Items returns a copy of the current items.
func (l *TodoList) Items() []Todo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.items)
}

// Set replaces the items.
func (l *TodoList) Set(items []Todo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = slices.Clone(items)
}

// String renders the list with [ ], [~] and [x] markers.
func (l *TodoList) String() string {
	items := l.Items()
	if len(items) == 0 {
		return "(no todos)"
	}
	var b strings.Builder
	for i, t := range items {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch t.Status {
		case TodoCompleted:
			b.WriteString("[x] ")
		case TodoInProgress:
			b.WriteString("[~] ")
		default:
			b.WriteString("[ ] ")
		}
		b.WriteString(t.Content)
	}
	return b.String()
}

type todoArgs struct {
	Todos []Todo `json:"todos" jsonschema:"The complete updated list, replacing the previous one."`
}

// TodoWrite returns the todo_write tool, which replaces the items of list.
func TodoWrite(list *TodoList) tool.Tool {
	return tool.Func("todo_write",
		"Maintain a task list for multi-step work. Send the complete list each time. "+
			"Keep exactly one item in_progress while working, and mark items completed as soon as they are done.",
		func(ctx context.Context, in todoArgs) (*tool.Result, error) {
			for i, t := range in.Todos {
				if strings.TrimSpace(t.Content) == "" {
					return tool.Errorf("todo %d has no content", i+1), nil
				}
				switch t.Status {
				case TodoPending, TodoInProgress, TodoCompleted:
				default:
					return tool.Errorf("todo %d has invalid status %q", i+1, t.Status), nil
				}
			}
			list.Set(in.Todos)
			return tool.Text(list.String()), nil
		})
}
