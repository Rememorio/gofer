package humaninput

import (
	"errors"
	"fmt"

	"github.com/Rememorio/gofer/internal/domain"
)

// ThreadState is deterministically reconstructed from durable messages.
type ThreadState struct {
	Requests            map[string]Request  `json:"requests"`
	OpenRequests        []Request           `json:"open_requests"`
	AnsweredResponses   map[string]Response `json:"answered_responses"`
	LatestOpenRequestID string              `json:"latest_open_request_id,omitempty"`
}

// State derives pending and answered requests without mutable side storage.
func State(messages []domain.Message) ThreadState {
	state, order := deriveState(messages)
	populateOpenRequests(&state, order)
	return state
}

func deriveState(messages []domain.Message) (ThreadState, []string) {
	state := ThreadState{
		Requests: make(map[string]Request), AnsweredResponses: make(map[string]Response),
	}
	order := make([]string, 0)
	for _, message := range messages {
		registerMessageRequests(&state, &order, message)
		applyHistoricalUserMessage(&state, order, message)
	}
	return state, order
}

func registerMessageRequests(state *ThreadState, order *[]string, message domain.Message) {
	for _, content := range message.Content {
		if content.Kind != domain.ContentToolResult || content.ToolResult == nil {
			continue
		}
		request, ok := RequestFromOutput(content.ToolResult.Output)
		if !ok {
			continue
		}
		if _, exists := state.Requests[request.RequestID]; !exists {
			*order = append(*order, request.RequestID)
		}
		state.Requests[request.RequestID] = request
	}
}

func applyHistoricalUserMessage(state *ThreadState, order []string, message domain.Message) {
	if message.Role != domain.RoleUser {
		return
	}
	if response, ok := ResponseFromMessage(message); ok {
		request, exists := state.Requests[response.RequestID]
		if exists && request.Source == response.Source {
			state.AnsweredResponses[response.RequestID] = response
		}
		return
	}
	if !Hidden(message) {
		closeLatestOpen(state, order, responseFromText(message))
	}
}

func populateOpenRequests(state *ThreadState, order []string) {
	for _, requestID := range order {
		request := state.Requests[requestID]
		if _, answered := state.AnsweredResponses[requestID]; answered {
			continue
		}
		state.OpenRequests = append(state.OpenRequests, request)
		state.LatestOpenRequestID = requestID
	}
}

// ValidateIncoming rejects stale, replayed, mismatched, and unknown structured
// responses before they enter the durable conversation.
func ValidateIncoming(history, incoming []domain.Message) error {
	state, order := deriveState(history)
	for _, message := range incoming {
		registerMessageRequests(&state, &order, message)
		if err := validateIncomingMessage(&state, order, message); err != nil {
			return err
		}
	}
	return nil
}

func validateIncomingMessage(state *ThreadState, order []string, message domain.Message) error {
	if message.Role != domain.RoleUser {
		return nil
	}
	response, structured := ResponseFromMessage(message)
	if !structured && message.Metadata[ResponseMetadataKey] != "" {
		return fmt.Errorf("%w: malformed response metadata", ErrInvalidResponse)
	}
	if !structured {
		if !Hidden(message) {
			closeLatestOpen(state, order, responseFromText(message))
		}
		return nil
	}
	request, exists := state.Requests[response.RequestID]
	if !exists || request.Source != response.Source {
		return fmt.Errorf("%w: request is unknown", ErrInvalidResponse)
	}
	if _, answered := state.AnsweredResponses[response.RequestID]; answered {
		return fmt.Errorf("%w: request was already answered", ErrInvalidResponse)
	}
	if err := validateResponseChoice(request, response); err != nil {
		return err
	}
	state.AnsweredResponses[response.RequestID] = response
	return nil
}

func validateResponseChoice(request Request, response Response) error {
	if response.ResponseKind == "text" {
		return nil
	}
	for _, option := range request.Options {
		if option.ID == response.OptionID && option.Value == response.Value {
			return nil
		}
	}
	return fmt.Errorf("%w: option does not belong to request", ErrInvalidResponse)
}

func closeLatestOpen(state *ThreadState, order []string, response Response) {
	for index := len(order) - 1; index >= 0; index-- {
		requestID := order[index]
		if _, answered := state.AnsweredResponses[requestID]; answered {
			continue
		}
		request := state.Requests[requestID]
		response.Version, response.Kind = 1, "human_input_response"
		response.Source, response.RequestID, response.ResponseKind = request.Source, requestID, "text"
		if response.Value == "" {
			response.Value = "User replied in the next turn."
		}
		state.AnsweredResponses[requestID] = response
		return
	}
}

func responseFromText(message domain.Message) Response {
	for _, content := range message.Content {
		if content.Kind == domain.ContentText {
			return Response{Value: content.Text}
		}
	}
	return Response{}
}

// IsInvalidResponse reports protocol-level response validation failures.
func IsInvalidResponse(err error) bool { return errors.Is(err, ErrInvalidResponse) }
