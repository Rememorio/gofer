package policy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestStaticPolicyUsesFirstMatchingRule(t *testing.T) {
	t.Parallel()

	policy, err := NewStatic(DecisionDeny,
		Rule{Tool: "read_*", Effect: EffectRead, Resource: "/mnt/user-data/**", Decision: DecisionAllow},
		Rule{Tool: "read_file", Effect: EffectRead, Resource: "/mnt/user-data/uploads/**", Decision: DecisionDeny},
		Rule{Tool: "delete_*", Decision: DecisionRequireApproval},
	)
	if err != nil {
		t.Fatalf("NewStatic(): %v", err)
	}
	if err := policy.Authorize(context.Background(), Action{
		Tool: "read_file", Effect: EffectRead, Resource: "/mnt/user-data/uploads/a.txt",
	}); err != nil {
		t.Fatalf("Authorize(first match): %v", err)
	}
	if err := policy.Authorize(context.Background(), Action{
		Tool: "write_file", Effect: EffectWrite, Resource: "/mnt/user-data/workspace/a.txt",
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("Authorize(default deny) error = %v, want ErrDenied", err)
	}
	if err := policy.Authorize(context.Background(), Action{
		Tool: "delete_file", Effect: EffectDestructive, Resource: "/mnt/user-data/workspace/a.txt",
	}); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("Authorize(approval) error = %v, want ErrApprovalRequired", err)
	}
}

func TestStaticPolicyValidationAndContext(t *testing.T) {
	t.Parallel()

	if _, err := NewStatic("unknown"); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("NewStatic(decision) error = %v, want ErrInvalidPolicy", err)
	}
	invalidRules := []Rule{
		{Decision: "unknown"},
		{Effect: "unknown", Decision: DecisionAllow},
		{Tool: "[", Decision: DecisionAllow},
		{Resource: "[", Decision: DecisionAllow},
	}
	for _, rule := range invalidRules {
		if _, err := NewStatic(DecisionDeny, rule); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("NewStatic(%#v) error = %v, want ErrInvalidPolicy", rule, err)
		}
	}
	policy, err := NewStatic(DecisionAllow)
	if err != nil {
		t.Fatalf("NewStatic(): %v", err)
	}
	if err := policy.Authorize(context.Background(), Action{}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("Authorize(invalid) error = %v, want ErrInvalidPolicy", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := policy.Authorize(ctx, Action{Tool: "x", Effect: EffectRead}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Authorize(cancelled) error = %v, want context.Canceled", err)
	}
	var nilPolicy *Static
	if err := nilPolicy.Authorize(context.Background(), Action{Tool: "x", Effect: EffectRead}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("nil Authorize() error = %v, want ErrInvalidPolicy", err)
	}
}

func TestMiddlewareAuthorizesResources(t *testing.T) {
	t.Parallel()

	recorder := &recordingAuthorizer{}
	descriptors := map[string]Descriptor{
		"read_file":     {Effect: EffectRead, ResourceFields: []string{"path"}},
		"present_files": {Effect: EffectRead, ResourceFields: []string{"filepaths"}},
	}
	middleware, err := NewMiddleware(recorder, descriptors)
	if err != nil {
		t.Fatalf("NewMiddleware(): %v", err)
	}
	descriptors["read_file"] = Descriptor{Effect: EffectWrite}
	calls := []domain.ToolCall{
		{ID: "1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/mnt/user-data/workspace/a"}`)},
		{ID: "2", Name: "present_files", Arguments: json.RawMessage(`{"filepaths":["/mnt/user-data/outputs/a","/mnt/user-data/outputs/b"]}`)},
		{ID: "3", Name: "unknown", Arguments: json.RawMessage(`{}`)},
	}
	for _, call := range calls {
		if err := middleware.BeforeTool(context.Background(), call); err != nil {
			t.Fatalf("BeforeTool(%s): %v", call.Name, err)
		}
	}
	want := []Action{
		{Tool: "read_file", Effect: EffectRead, Resource: "/mnt/user-data/workspace/a"},
		{Tool: "present_files", Effect: EffectRead, Resource: "/mnt/user-data/outputs/a"},
		{Tool: "present_files", Effect: EffectRead, Resource: "/mnt/user-data/outputs/b"},
		{Tool: "unknown", Effect: EffectExecute},
	}
	if len(recorder.actions) != len(want) {
		t.Fatalf("actions = %#v, want %#v", recorder.actions, want)
	}
	for index := range want {
		if recorder.actions[index] != want[index] {
			t.Fatalf("actions = %#v, want %#v", recorder.actions, want)
		}
	}
	if err := middleware.BeforeModel(context.Background(), &model.Request{}); err != nil {
		t.Fatalf("BeforeModel(): %v", err)
	}
	if err := middleware.AfterModel(context.Background(), model.Response{}); err != nil {
		t.Fatalf("AfterModel(): %v", err)
	}
	if err := middleware.AfterTool(context.Background(), domain.ToolCall{}, domain.ToolResult{}); err != nil {
		t.Fatalf("AfterTool(): %v", err)
	}
}

func TestMiddlewareValidationAndArgumentErrors(t *testing.T) {
	t.Parallel()

	if _, err := NewMiddleware(nil, nil); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("NewMiddleware(nil) error = %v, want ErrInvalidPolicy", err)
	}
	recorder := &recordingAuthorizer{}
	invalid := []map[string]Descriptor{
		{"": {Effect: EffectRead}},
		{"x": {Effect: "unknown"}},
		{"x": {Effect: EffectRead, ResourceFields: []string{""}}},
	}
	for _, descriptors := range invalid {
		if _, err := NewMiddleware(recorder, descriptors); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("NewMiddleware(%#v) error = %v, want ErrInvalidPolicy", descriptors, err)
		}
	}
	middleware, err := NewMiddleware(recorder, map[string]Descriptor{
		"x": {Effect: EffectRead, ResourceFields: []string{"path"}},
	})
	if err != nil {
		t.Fatalf("NewMiddleware(): %v", err)
	}
	arguments := []json.RawMessage{
		json.RawMessage(`{`), json.RawMessage(`{}`), json.RawMessage(`{"path":1}`),
		json.RawMessage(`{"path":["ok",1]}`),
	}
	for _, value := range arguments {
		if err := middleware.BeforeTool(context.Background(), domain.ToolCall{Name: "x", Arguments: value}); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("BeforeTool(%s) error = %v, want ErrInvalidPolicy", value, err)
		}
	}
	var nilMiddleware *Middleware
	if err := nilMiddleware.BeforeTool(context.Background(), domain.ToolCall{}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("nil BeforeTool() error = %v, want ErrInvalidPolicy", err)
	}
}

type recordingAuthorizer struct{ actions []Action }

func (authorizer *recordingAuthorizer) Authorize(_ context.Context, action Action) error {
	authorizer.actions = append(authorizer.actions, action)
	return nil
}
