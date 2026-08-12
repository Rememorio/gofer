package uploads

import (
	"context"
	"errors"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestContextMiddlewareInjectsWithoutMutatingDurableMessage(t *testing.T) {
	t.Parallel()
	middleware := NewContextMiddleware("<current_uploads>files</current_uploads>")
	original := domain.Message{Role: domain.RoleUser, Content: []domain.Content{
		{Kind: domain.ContentImage, URL: "https://example.test/a.png", MediaType: "image/png"},
		{Kind: domain.ContentText, Text: "question"},
	}}
	request := model.Request{Messages: []domain.Message{{Role: domain.RoleAssistant}, original}}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	want := "<current_uploads>files</current_uploads>\n\nquestion"
	if request.Messages[1].Content[1].Text != want || original.Content[1].Text != "question" {
		t.Fatalf("request/original = %#v / %#v", request.Messages[1], original)
	}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil || request.Messages[1].Content[1].Text != want {
		t.Fatalf("idempotent BeforeModel() = %q, %v", request.Messages[1].Content[1].Text, err)
	}
}

func TestContextMiddlewareHandlesAbsentTextAndErrors(t *testing.T) {
	t.Parallel()
	if NewContextMiddleware("  ") != nil {
		t.Fatal("empty section returned middleware")
	}
	middleware := NewContextMiddleware("section")
	requests := []*model.Request{
		{},
		{Messages: []domain.Message{{Role: domain.RoleAssistant}}},
		{Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.Content{{Kind: domain.ContentImage}}}}},
	}
	for _, request := range requests {
		if err := middleware.BeforeModel(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if err := (*ContextMiddleware)(nil).BeforeModel(context.Background(), &model.Request{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil middleware error = %v", err)
	}
	if err := middleware.BeforeModel(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil request error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := middleware.BeforeModel(cancelled, &model.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
}
