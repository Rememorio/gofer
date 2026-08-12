package model

import (
	"context"
	"errors"
	"io"
	"sync"
)

// Scripted is a deterministic Provider for tests and contract fixtures.
type Scripted struct {
	mu        sync.Mutex
	Responses [][]Chunk
	Requests  []Request
	Err       error
}

// Stream records request and returns the next scripted response.
func (provider *Scripted) Stream(ctx context.Context, request Request) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.Requests = append(provider.Requests, request)
	if provider.Err != nil {
		return nil, provider.Err
	}
	if len(provider.Responses) == 0 {
		return nil, errors.New("scripted model has no response")
	}
	chunks := append([]Chunk(nil), provider.Responses[0]...)
	provider.Responses = provider.Responses[1:]
	return &sliceStream{chunks: chunks}, nil
}

type sliceStream struct {
	mu     sync.Mutex
	chunks []Chunk
	closed bool
}

func (stream *sliceStream) Recv() (Chunk, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed || len(stream.chunks) == 0 {
		return Chunk{}, io.EOF
	}
	chunk := stream.chunks[0]
	stream.chunks = stream.chunks[1:]
	return chunk, nil
}

func (stream *sliceStream) Close() error {
	stream.mu.Lock()
	stream.closed = true
	stream.mu.Unlock()
	return nil
}
