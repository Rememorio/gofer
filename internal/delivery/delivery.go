package delivery

import (
	"errors"
	"sort"
)

const (
	// EventCategory is the stable category for terminal delivery receipts.
	EventCategory = "outputs"
	// ToolPresentFiles is the tool that explicitly delivers output artifacts.
	ToolPresentFiles = "present_files"
)

var (
	// ErrIncomplete identifies changed outputs not covered by present_files.
	ErrIncomplete = errors.New("Artifact delivery incomplete: no produced output artifact was presented")
	// ErrReceiptFailed identifies an unverifiable delivery due to journal failure.
	ErrReceiptFailed = errors.New("Artifact delivery verification failed: terminal delivery receipt could not be persisted")
)

// Verification describes how produced outputs are matched to presentation.
type Verification struct {
	Source      string `json:"source"`
	Requirement string `json:"requirement"`
}

// Verdict is present only when a run created or modified output files.
type Verdict struct {
	Verification   Verification `json:"verification"`
	ProducedPaths  []string     `json:"produced_paths"`
	PresentedPaths []string     `json:"presented_paths"`
	MatchedPaths   []string     `json:"matched_paths"`
	Stage          string       `json:"stage"`
	Satisfied      bool         `json:"satisfied"`
}

// Receipt is the immutable terminal artifact-delivery fact for one run.
type Receipt struct {
	Presented int                 `json:"presented"`
	Paths     []string            `json:"paths"`
	ByTool    map[string][]string `json:"by_tool"`
	*Verdict
}

// EventPayload supplies category and content through Gofer's typed journal.
type EventPayload struct {
	Category string  `json:"category"`
	Content  Receipt `json:"content"`
}

// EmptyReceipt returns the stable zero-presentation receipt for ordinary chat
// and failures that stop before workspace verification is available.
func EmptyReceipt() Receipt {
	return Receipt{Paths: []string{}, ByTool: map[string][]string{}}
}

// CompletionError applies terminal delivery semantics to a persisted receipt.
// Receipt persistence remains best effort for ordinary chat runs with no
// produced outputs.
func CompletionError(receipt Receipt, persistErr error) error {
	if receipt.Verdict == nil {
		return nil
	}
	var result error
	if persistErr != nil {
		result = errors.Join(result, ErrReceiptFailed)
	}
	if !receipt.Satisfied {
		result = errors.Join(result, ErrIncomplete)
	}
	return result
}

func newVerdict(produced, presented []string) *Verdict {
	matched := make([]string, 0, len(produced))
	for _, producedPath := range produced {
		if coveredByAny(presented, producedPath) {
			matched = append(matched, producedPath)
		}
	}
	satisfied := len(matched) > 0
	stage := "not_started"
	if satisfied {
		stage = "presented"
	} else if len(presented) > 0 {
		stage = "mismatched"
	}
	return &Verdict{
		Verification:  Verification{Source: "outputs_changed", Requirement: "present_files_matches_produced_output"},
		ProducedPaths: cloneSorted(produced), PresentedPaths: append([]string(nil), presented...),
		MatchedPaths: matched, Stage: stage, Satisfied: satisfied,
	}
}

func coveredByAny(presented []string, produced string) bool {
	for _, candidate := range presented {
		candidate = trimTrailingSlash(candidate)
		if candidate != "" && (produced == candidate || len(produced) > len(candidate) && produced[:len(candidate)+1] == candidate+"/") {
			return true
		}
	}
	return false
}

func trimTrailingSlash(value string) string {
	for len(value) > 1 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}

func cloneSorted(values []string) []string {
	cloned := append([]string(nil), values...)
	sort.Strings(cloned)
	return cloned
}
