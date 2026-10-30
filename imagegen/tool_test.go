package imagegen_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/artifact"
	"github.com/nostosx/agentx/imagegen"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

type artifactFixture struct {
	saved []*artifact.Artifact
	fail  error
}

func (s *artifactFixture) Put(_ context.Context, value *artifact.Artifact) error {
	if s.fail != nil {
		return s.fail
	}
	s.saved = append(s.saved, value)
	return nil
}
func (s *artifactFixture) Get(context.Context, artifact.Key) (*artifact.Artifact, error) {
	return nil, artifact.ErrNotFound
}
func (s *artifactFixture) Delete(context.Context, artifact.Key) error { return nil }

func TestToolArtifacts(t *testing.T) {
	t.Parallel()
	store := &artifactFixture{}
	calls := 0
	g := generatorFunc(func(context.Context, *imagegen.Request) (*imagegen.Response, error) {
		calls++
		return imageResponse(), nil
	})
	tl := imagegen.Tool(g, imagegen.ToolOptions{Artifacts: store, ArtifactScope: "trusted-user"})
	out, err := tl.Call(t.Context(), json.RawMessage(`{"prompt":"draw","artifact_scope":"untrusted"}`))
	if err != nil || len(store.saved) != 1 || store.saved[0].Scope != "trusted-user" || string(store.saved[0].Data) != "image" || store.saved[0].ID == "" {
		t.Fatalf("saved=%+v err=%v", store.saved, err)
	}
	refs, ok := out.Metadata["artifacts"].([]artifact.Key)
	if !ok || len(refs) != 1 || refs[0] != store.saved[0].Key {
		t.Fatal("artifact references not returned")
	}
	store.fail = errors.New("storage failure")
	_, err = tl.Call(t.Context(), json.RawMessage(`{"prompt":"draw"}`))
	if !errors.Is(err, store.fail) {
		t.Fatalf("persistence failure=%v", err)
	}
	if failure, ok := errors.AsType[*imagegen.ArtifactError](err); !ok || failure.Response == nil || string(failure.Response.Images[0].Part.Data) != "image" {
		t.Fatal("generated output lost after storage failure")
	}
	if calls != 2 {
		t.Fatal("storage failure caused regeneration")
	}
	if _, err := imagegen.Tool(g, imagegen.ToolOptions{Artifacts: store}).Call(t.Context(), json.RawMessage(`{"prompt":"draw"}`)); err == nil || calls != 2 {
		t.Fatal("missing trusted scope accepted")
	}
}

type generatorFunc func(context.Context, *imagegen.Request) (*imagegen.Response, error)

func (f generatorFunc) Generate(ctx context.Context, req *imagegen.Request) (*imagegen.Response, error) {
	return f(ctx, req)
}

type editingGenerator struct {
	generatorFunc
	edit func(context.Context, *imagegen.EditRequest) (*imagegen.Response, error)
}

func (g editingGenerator) Edit(ctx context.Context, req *imagegen.EditRequest) (*imagegen.Response, error) {
	return g.edit(ctx, req)
}

func imageResponse() *imagegen.Response {
	return &imagegen.Response{Provider: "fixture", Model: "image", RequestID: "request-1",
		Images: []imagegen.Image{{Part: model.ImagePart("image/png", []byte("image"))}},
		Usage:  &imagegen.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}}
}

func TestTool(t *testing.T) {
	t.Parallel()
	defaults := imagegen.Request{Model: "trusted-model", N: 1, SafetyIdentifier: "hashed-user",
		ProviderOptions: map[string]map[string]any{"fixture": {"moderation": "auto"}}}
	g := generatorFunc(func(_ context.Context, req *imagegen.Request) (*imagegen.Response, error) {
		if req.Prompt != "draw a cat" || req.N != 2 || req.Size != "1024x1024" || req.Model != "trusted-model" || req.SafetyIdentifier != "hashed-user" {
			t.Fatalf("request = %+v", req)
		}
		if req.ProviderOptions["fixture"]["moderation"] != "auto" {
			t.Fatal("shared provider options were mutated")
		}
		req.ProviderOptions["fixture"]["moderation"] = "changed"
		return imageResponse(), nil
	})
	tl := imagegen.Tool(g, imagegen.ToolOptions{Request: defaults, MaxImages: 2}, tool.RequireApproval())
	info := tl.Info()
	if info.ReadOnly || info.Concurrent || !info.RequiresApproval || info.Name != "generate_image" || info.Parameters.Properties["images"] != nil {
		t.Fatalf("tool info = %+v", info)
	}
	args := json.RawMessage(`{"prompt":"draw a cat","n":2,"size":"1024x1024","model":"untrusted","safety_identifier":"untrusted"}`)
	for range 2 {
		out, err := tl.Call(t.Context(), args)
		if err != nil || out == nil || len(out.Parts) != 1 || out.Parts[0].Type != model.PartImage || out.Metadata["request_id"] != "request-1" {
			t.Fatalf("Call = %+v, %v", out, err)
		}
		if _, err := json.Marshal(out.Metadata); err != nil {
			t.Fatal(err)
		}
	}
	if defaults.ProviderOptions["fixture"]["moderation"] != "auto" {
		t.Fatal("input options mutated")
	}
}

func TestToolEdit(t *testing.T) {
	t.Parallel()
	var resolved []string
	g := editingGenerator{edit: func(_ context.Context, req *imagegen.EditRequest) (*imagegen.Response, error) {
		if len(req.Images) != 1 || req.Mask == nil || req.Markup == nil || req.Prompt != "edit" {
			t.Fatalf("edit = %+v", req)
		}
		return imageResponse(), nil
	}}
	tl := imagegen.Tool(g, imagegen.ToolOptions{ResolveImage: func(_ context.Context, ref string) (model.Part, error) {
		resolved = append(resolved, ref)
		return model.ImagePart("image/png", []byte(ref)), nil
	}})
	if tl.Info().Parameters.Properties["images"] == nil {
		t.Fatal("editing absent from schema")
	}
	_, err := tl.Call(t.Context(), json.RawMessage(`{"prompt":"edit","images":["source"],"mask":"same","markup":"same"}`))
	if err != nil || !reflect.DeepEqual(resolved, []string{"source", "same", "same"}) {
		t.Fatalf("resolved = %v, err = %v", resolved, err)
	}
}

func TestToolErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("provider failed")
	for _, tc := range []struct {
		name string
		g    imagegen.Generator
		opts imagegen.ToolOptions
		args string
	}{
		{name: "nil generator", args: `{"prompt":"x"}`},
		{name: "empty prompt", args: `{"prompt":" "}`, g: generatorFunc(func(context.Context, *imagegen.Request) (*imagegen.Response, error) {
			t.Fatal("called")
			return nil, nil
		})},
		{name: "negative count", args: `{"prompt":"x","n":-1}`, g: generatorFunc(nil)},
		{name: "count bound", args: `{"prompt":"x","n":2}`, g: generatorFunc(nil)},
		{name: "mask without images", args: `{"prompt":"x","mask":"m"}`, g: generatorFunc(nil)},
		{name: "editing unavailable", args: `{"prompt":"x","images":["i"]}`, g: generatorFunc(nil)},
		{name: "nil response", args: `{"prompt":"x"}`, g: generatorFunc(func(context.Context, *imagegen.Request) (*imagegen.Response, error) { return nil, nil })},
		{name: "empty response", args: `{"prompt":"x"}`, g: generatorFunc(func(context.Context, *imagegen.Request) (*imagegen.Response, error) { return &imagegen.Response{}, nil })},
		{name: "empty image", args: `{"prompt":"x"}`, g: generatorFunc(func(context.Context, *imagegen.Request) (*imagegen.Response, error) {
			return &imagegen.Response{Images: []imagegen.Image{{}}}, nil
		})},
		{name: "provider failure", args: `{"prompt":"x"}`, g: generatorFunc(func(context.Context, *imagegen.Request) (*imagegen.Response, error) { return nil, sentinel })},
		{name: "resolver failure", args: `{"prompt":"x","images":["i"]}`, g: editingGenerator{}, opts: imagegen.ToolOptions{ResolveImage: func(context.Context, string) (model.Part, error) { return model.Part{}, sentinel }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := imagegen.Tool(tc.g, tc.opts).Call(t.Context(), json.RawMessage(tc.args))
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.name == "provider failure" || tc.name == "resolver failure" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("lost error chain: %v", err)
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := imagegen.Tool(generatorFunc(nil), imagegen.ToolOptions{}).Call(ctx, json.RawMessage(`{"prompt":"x"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestAgentImageToolRoundTrip(t *testing.T) {
	t.Parallel()
	g := generatorFunc(func(context.Context, *imagegen.Request) (*imagegen.Response, error) { return imageResponse(), nil })
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "image-1", Name: "generate_image", Args: map[string]any{"prompt": "draw a cat"}}),
		modeltest.Reply("Here is the image."),
	)
	a := agent.MustNew("artist", agent.WithModel(m), agent.WithTools(imagegen.Tool(g, imagegen.ToolOptions{})))
	answer, err := a.Ask(t.Context(), "Draw a cat")
	if err != nil || answer != "Here is the image." {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	requests := m.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	found := false
	for _, msg := range requests[1].Messages {
		for _, result := range msg.ToolResults() {
			if result.CallID == "image-1" && len(result.Parts) == 1 && string(result.Parts[0].Data) == "image" && result.Metadata["request_id"] == "request-1" {
				found = true
				data, err := json.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				var restored model.Message
				if err := json.Unmarshal(data, &restored); err != nil {
					t.Fatal(err)
				}
				saved := restored.ToolResults()[0]
				if string(saved.Parts[0].Data) != "image" || saved.Metadata["request_id"] != "request-1" {
					t.Fatal("image bytes or metadata lost after conversation serialization")
				}
			}
		}
	}
	if !found {
		t.Fatal("generated image or metadata lost during agent tool roundtrip")
	}
}
