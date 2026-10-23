package middleware_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/nostosx/agentx/middleware"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestWrapOrderAndFuncsDelegation(t *testing.T) {
	t.Parallel()
	var order []string
	wrap := func(name string) middleware.Model {
		return func(next model.Model) model.Model {
			return &middleware.Funcs{Next: next, GenerateFunc: func(ctx context.Context, req *model.Request) (*model.Response, error) {
				order = append(order, name+" before")
				res, err := next.Generate(ctx, req)
				order = append(order, name+" after")
				return res, err
			}}
		}
	}
	base := modeltest.New(func(*model.Request) (*model.Response, error) {
		order = append(order, "call")
		return &model.Response{Message: model.AssistantText("done")}, nil
	}, modeltest.Reply("stream"))
	wrapped := middleware.Wrap(base, wrap("a"), wrap("b"))
	res, err := wrapped.Generate(t.Context(), &model.Request{})
	if err != nil || res.Text() != "done" || !reflect.DeepEqual(order, []string{"a before", "b before", "call", "b after", "a after"}) {
		t.Fatalf("Generate=%v,%v order=%v", res, err, order)
	}
	var done bool
	for ev, err := range wrapped.Stream(t.Context(), &model.Request{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == model.StreamDone {
			done = true
		}
	}
	if !done {
		t.Fatal("Funcs did not delegate streaming")
	}
}
