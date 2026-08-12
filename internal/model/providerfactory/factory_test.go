package providerfactory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/model/anthropicmessages"
	"github.com/Rememorio/gofer/internal/model/openaichat"
)

func TestOpenSupportedProviders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config Config
		check  func(any) bool
	}{
		{name: "OpenAI Chat", config: Config{Provider: "openai", APIKey: "key"}, check: func(value any) bool {
			_, ok := value.(*openaichat.Provider)
			return ok
		}},
		{name: "Anthropic Messages", config: Config{Provider: "anthropic", AuthToken: "token"}, check: func(value any) bool {
			_, ok := value.(*anthropicmessages.Provider)
			return ok
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider, err := Open(test.config)
			if err != nil || !test.check(provider) {
				t.Fatalf("Open() = %T, %v", provider, err)
			}
		})
	}
}

func message(t *testing.T) domain.Message {
	t.Helper()
	message, err := domain.NewTextMessage(domain.RoleUser, "hello", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestOpenRejectsInvalidProviderConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		config Config
		want   error
	}{
		{config: Config{}, want: ErrInvalidConfig},
		{config: Config{Provider: "openai", MaxTokens: -1}, want: ErrInvalidConfig},
		{config: Config{Provider: "other"}, want: ErrUnsupported},
		{config: Config{Provider: "openai", AuthToken: "token"}, want: ErrInvalidConfig},
		{config: Config{Provider: "anthropic", APIKey: " key"}, want: anthropicmessages.ErrInvalidConfig},
	}
	for _, test := range tests {
		if _, err := Open(test.config); !errors.Is(err, test.want) {
			t.Fatalf("Open(%#v) error = %v, want %v", test.config, err, test.want)
		}
	}
}

func TestProviderDefaultMaxTokens(t *testing.T) {
	t.Parallel()
	base := &model.Scripted{Responses: [][]model.Chunk{{{Kind: model.ChunkDone, StopReason: model.StopEndTurn}}}}
	provider := withDefaultMaxTokens(base, 4096)
	request := model.Request{Model: "test", Messages: []domain.Message{message(t)}}
	stream, err := provider.Stream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if len(base.Requests) != 1 || base.Requests[0].MaxTokens != 4096 {
		t.Fatalf("request = %#v", base.Requests)
	}

	base = &model.Scripted{Responses: [][]model.Chunk{{{Kind: model.ChunkDone, StopReason: model.StopEndTurn}}}}
	provider = withDefaultMaxTokens(base, 4096)
	request.MaxTokens = 128
	stream, err = provider.Stream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if base.Requests[0].MaxTokens != 128 {
		t.Fatalf("explicit max tokens = %d", base.Requests[0].MaxTokens)
	}
	if withDefaultMaxTokens(nil, 1) != nil || withDefaultMaxTokens(base, 0) != base {
		t.Fatal("disabled defaults changed provider")
	}
}
