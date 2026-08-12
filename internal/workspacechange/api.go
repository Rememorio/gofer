package workspacechange

import (
	"encoding/json"
	"fmt"

	"github.com/Rememorio/gofer/internal/event"
)

// NewEventPayload wraps one result in the stable journal event envelope.
func NewEventPayload(result Result) EventPayload {
	count := result.Summary.Created + result.Summary.Modified + result.Summary.Deleted + result.Summary.SymlinkCreated
	return EventPayload{
		Category:         EventCategory,
		Content:          fmt.Sprintf("%d %s changed +%d -%d", count, pluralFiles(count), result.Summary.Additions, result.Summary.Deletions),
		WorkspaceChanges: result,
	}
}

// ResponseFromEvents returns the latest durable review with optional detail
// elision. Malformed legacy payloads safely degrade to an unavailable result.
func ResponseFromEvents(events []event.Event, includeFiles, includeDiff bool) Response {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Kind != event.WorkspaceChanges {
			continue
		}
		result, ok := decodeResult(events[index].Data)
		if !ok {
			return EmptyResponse()
		}
		return filterResponse(result, includeFiles, includeDiff)
	}
	return EmptyResponse()
}

// EmptyResponse returns the stable no-review response shape.
func EmptyResponse() Response {
	return Response{Available: false, Result: Result{Version: resultVersion, Files: []FileChange{}}}
}

func decodeResult(data json.RawMessage) (Result, bool) {
	var payload EventPayload
	if err := json.Unmarshal(data, &payload); err == nil && payload.WorkspaceChanges.Version > 0 {
		return payload.WorkspaceChanges, true
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil || result.Version == 0 {
		return Result{}, false
	}
	return result, true
}

func filterResponse(result Result, includeFiles, includeDiff bool) Response {
	result.Files = append([]FileChange(nil), result.Files...)
	if !includeFiles {
		result.Files = []FileChange{}
	} else if !includeDiff {
		for index := range result.Files {
			result.Files[index].Diff = ""
		}
	}
	if result.Files == nil {
		result.Files = []FileChange{}
	}
	return Response{Available: true, Result: result}
}

func pluralFiles(count int) string {
	if count == 1 {
		return "file"
	}
	return "files"
}
