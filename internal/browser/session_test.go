package browser

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestAutomationNavigateActAndCapture(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{snapshots: []Snapshot{
		{URL: "https://example.com", Title: "Page", Text: "Untrusted text", Elements: []Element{
			{Ref: 2, Tag: "button", Name: "Save"}, {Ref: 1, Tag: "input", Role: "textbox", Name: "Query"},
		}},
		{URL: "https://example.com/next", Title: "Next", Elements: []Element{{Ref: 3, Tag: "a", Name: "Back"}}},
		{URL: "https://example.com/typed", Title: "Typed", Elements: []Element{{Ref: 4, Tag: "button"}}},
		{URL: "https://example.com/scrolled", Title: "Scrolled"},
		{URL: "https://example.com", Title: "Page"},
	}}
	automation := newTestAutomation(t, runner)
	snapshot, err := automation.Navigate(context.Background(), "https://example.com")
	assertNavigation(t, snapshot, runner, err)
	snapshot, err = automation.Click(context.Background(), 2)
	assertClick(t, snapshot, runner, err)
	snapshot, err = automation.Type(context.Background(), 3, "hello", true)
	assertType(t, snapshot, runner, err)
	if _, err := automation.Scroll(context.Background(), 10, 20); err != nil ||
		!strings.Contains(runner.expressions[len(runner.expressions)-2], "left:10") {
		t.Fatalf("Scroll() error = %v, expressions=%#v", err, runner.expressions)
	}
	if _, err := automation.Back(context.Background()); err != nil ||
		runner.expressions[len(runner.expressions)-2] != "history.back()" {
		t.Fatalf("Back() error = %v, expressions=%#v", err, runner.expressions)
	}
	image, err := automation.Screenshot(context.Background(), true)
	if err != nil || string(image) != "png" || !runner.fullPage {
		t.Fatalf("Screenshot() = %q, %v", image, err)
	}
}

func assertNavigation(t *testing.T, snapshot Snapshot, runner *fakeRunner, err error) {
	t.Helper()
	if err != nil || !reflect.DeepEqual(elementRefs(snapshot.Elements), []int{1, 2}) ||
		runner.navigated != "https://example.com" {
		t.Fatalf("Navigate() = %#v, runner=%#v, err=%v", snapshot, runner, err)
	}
	if rendered := snapshot.Render(); !strings.Contains(rendered, "untrusted data") ||
		!strings.Contains(rendered, "[1] textbox: Query") {
		t.Fatalf("Render() = %q", rendered)
	}
}

func assertClick(t *testing.T, snapshot Snapshot, runner *fakeRunner, err error) {
	t.Helper()
	if err != nil || runner.clicked != `[data-gofer-ref="2"]` || snapshot.Title != "Next" {
		t.Fatalf("Click() = %#v, selector=%q, err=%v", snapshot, runner.clicked, err)
	}
}

func assertType(t *testing.T, snapshot Snapshot, runner *fakeRunner, err error) {
	t.Helper()
	if err != nil || runner.typedSelector != `[data-gofer-ref="3"]` || runner.typedText != "hello" ||
		!runner.submitted || snapshot.Title != "Typed" {
		t.Fatalf("Type() = %#v, runner=%#v, err=%v", snapshot, runner, err)
	}
}

func TestAutomationRejectsStaleRefsAndInvalidSnapshots(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{snapshots: []Snapshot{{
		URL: "https://example.com", Elements: []Element{{Ref: 1}, {Ref: 1}},
	}}}
	automation := newTestAutomation(t, runner)
	if _, err := automation.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot(duplicate refs) error = nil")
	}
	runner.snapshots = []Snapshot{{URL: "https://example.com", Elements: []Element{{Ref: 2}}}}
	runner.snapshotIndex = 0
	if _, err := automation.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot(): %v", err)
	}
	for _, ref := range []int{0, 1, -1} {
		if _, err := automation.Click(context.Background(), ref); !errors.Is(err, ErrInvalidReference) {
			t.Fatalf("Click(%d) error = %v", ref, err)
		}
	}
	if _, err := automation.Scroll(context.Background(), 0, 0); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("Scroll(zero) error = %v", err)
	}
	if _, err := automation.Scroll(context.Background(), 0, 100_001); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("Scroll(large) error = %v", err)
	}
}

func TestAutomationErrorsCancellationAndClose(t *testing.T) {
	t.Parallel()

	if _, err := NewAutomation(nil, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewAutomation(nil) error = %v", err)
	}
	var nilAutomation *Automation
	if _, err := nilAutomation.Navigate(context.Background(), "https://example.com"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Navigate() error = %v", err)
	}
	if _, err := nilAutomation.Screenshot(context.Background(), false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Screenshot() error = %v", err)
	}
	if err := nilAutomation.Close(); err != nil {
		t.Fatalf("nil Close(): %v", err)
	}
	runner := &fakeRunner{snapshots: []Snapshot{{URL: "https://example.com"}}}
	automation := newTestAutomation(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := automation.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot(cancelled) error = %v", err)
	}
	if err := automation.Close(); err != nil || runner.closeCalls != 1 {
		t.Fatalf("Close() = %v, calls=%d", err, runner.closeCalls)
	}
	if err := automation.Close(); err != nil || runner.closeCalls != 1 {
		t.Fatalf("Close(second) = %v, calls=%d", err, runner.closeCalls)
	}
	if _, err := automation.Snapshot(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Snapshot(closed) error = %v", err)
	}
	if _, err := automation.Screenshot(context.Background(), false); !errors.Is(err, ErrClosed) {
		t.Fatalf("Screenshot(closed) error = %v", err)
	}
}

func TestSnapshotValidationAndEmptyRender(t *testing.T) {
	t.Parallel()

	invalid := []Snapshot{
		{},
		{URL: "x", Text: strings.Repeat("x", 24_001)},
		{URL: "x", Elements: []Element{{Ref: 0}}},
	}
	for _, snapshot := range invalid {
		if err := validateSnapshot(snapshot); err == nil {
			t.Fatalf("validateSnapshot(%#v) error = nil", snapshot)
		}
	}
	rendered := (Snapshot{URL: "about:blank"}).Render()
	if !strings.Contains(rendered, "Interactive elements: none") {
		t.Fatalf("Render(empty) = %q", rendered)
	}
}

func newTestAutomation(t *testing.T, runner Runner) *Automation {
	t.Helper()
	guard, err := NewURLGuard(URLGuardConfig{Resolver: fakeResolver{addresses: map[string][]netip.Addr{
		"example.com": {netip.MustParseAddr("93.184.216.34")},
	}}})
	if err != nil {
		t.Fatalf("NewURLGuard(): %v", err)
	}
	automation, err := NewAutomation(runner, guard)
	if err != nil {
		t.Fatalf("NewAutomation(): %v", err)
	}
	return automation
}

type fakeRunner struct {
	snapshots     []Snapshot
	snapshotIndex int
	navigated     string
	clicked       string
	typedSelector string
	typedText     string
	submitted     bool
	expressions   []string
	fullPage      bool
	err           error
	closeCalls    int
}

func (runner *fakeRunner) Navigate(_ context.Context, rawURL string) error {
	runner.navigated = rawURL
	return runner.err
}

func (runner *fakeRunner) Evaluate(_ context.Context, expression string, output any) error {
	runner.expressions = append(runner.expressions, expression)
	if runner.err != nil {
		return runner.err
	}
	if snapshot, ok := output.(*Snapshot); ok {
		if runner.snapshotIndex >= len(runner.snapshots) {
			return errors.New("no fake snapshot")
		}
		*snapshot = cloneSnapshot(runner.snapshots[runner.snapshotIndex])
		runner.snapshotIndex++
	}
	return nil
}

func (runner *fakeRunner) Click(_ context.Context, selector string) error {
	runner.clicked = selector
	return runner.err
}

func (runner *fakeRunner) Type(_ context.Context, selector, text string, submit bool) error {
	runner.typedSelector, runner.typedText, runner.submitted = selector, text, submit
	return runner.err
}

func (runner *fakeRunner) Screenshot(_ context.Context, fullPage bool) ([]byte, error) {
	runner.fullPage = fullPage
	return []byte("png"), runner.err
}

func (runner *fakeRunner) Close() error {
	runner.closeCalls++
	return runner.err
}

func elementRefs(elements []Element) []int {
	refs := make([]int, len(elements))
	for index, element := range elements {
		refs[index] = element.Ref
	}
	return refs
}
