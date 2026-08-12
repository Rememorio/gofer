package providerfactory

import (
	"context"
	"errors"
	"fmt"

	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/model/anthropicmessages"
	"github.com/Rememorio/gofer/internal/model/openaichat"
)

var (
	// ErrInvalidConfig identifies a missing or internally inconsistent provider
	// factory configuration.
	ErrInvalidConfig = errors.New("invalid model provider config")
	// ErrUnsupported identifies an unknown provider kind.
	ErrUnsupported = errors.New("unsupported model provider")
)

// Config describes one provider endpoint without binding callers to a vendor
// SDK configuration type.
type Config struct {
	Provider  string
	APIKey    string
	AuthToken string
	BaseURL   string
	MaxTokens int
}

// Open constructs a normalized provider for the configured protocol.
func Open(config Config) (model.Provider, error) {
	if config.Provider == "" || config.MaxTokens < 0 {
		return nil, fmt.Errorf("%w: provider and non-negative token limit are required", ErrInvalidConfig)
	}
	switch config.Provider {
	case "openai":
		if config.AuthToken != "" {
			return nil, fmt.Errorf("%w: auth_token is unsupported by OpenAI Chat", ErrInvalidConfig)
		}
		provider, err := openaichat.New(openaichat.Config{APIKey: config.APIKey, BaseURL: config.BaseURL})
		return withDefaultMaxTokens(provider, config.MaxTokens), err
	case "anthropic":
		provider, err := anthropicmessages.New(anthropicmessages.Config{
			APIKey: config.APIKey, AuthToken: config.AuthToken,
			BaseURL: config.BaseURL,
		})
		return withDefaultMaxTokens(provider, config.MaxTokens), err
	default:
		return nil, fmt.Errorf("%w %q", ErrUnsupported, config.Provider)
	}
}

type defaultMaxTokensProvider struct {
	model.Provider
	maxTokens int
}

func withDefaultMaxTokens(provider model.Provider, maxTokens int) model.Provider {
	if provider == nil || maxTokens <= 0 {
		return provider
	}
	return &defaultMaxTokensProvider{Provider: provider, maxTokens: maxTokens}
}

func (provider *defaultMaxTokensProvider) Stream(ctx context.Context, request model.Request) (model.Stream, error) {
	if request.MaxTokens == 0 {
		request.MaxTokens = provider.maxTokens
	}
	return provider.Provider.Stream(ctx, request)
}
