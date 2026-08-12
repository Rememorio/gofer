package browser

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func TestChromeConfigAndFactory(t *testing.T) {
	t.Parallel()

	guard, _ := NewURLGuard(URLGuardConfig{Resolver: fakeResolver{addresses: map[string][]netip.Addr{
		"example.com": {netip.MustParseAddr("93.184.216.34")},
	}}})
	if _, err := NewChromeFactory(ChromeConfig{}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewChromeFactory(nil guard) error = %v", err)
	}
	factory, err := NewChromeFactory(ChromeConfig{}, guard)
	if err != nil || factory == nil {
		t.Fatalf("NewChromeFactory() = %v, %v", factory, err)
	}
	config := ChromeConfig{}
	if err := applyChromeDefaults(&config); err != nil {
		t.Fatalf("applyChromeDefaults(): %v", err)
	}
	if config.ViewportWidth != 1280 || config.ViewportHeight != 720 ||
		config.ActionTimeout != 30*time.Second || config.GuardWorkers != 64 {
		t.Fatalf("Chrome defaults = %#v", config)
	}
	invalid := []ChromeConfig{
		{ExecutablePath: "bad\x00path"},
		{RemoteURL: "relative"},
		{RemoteURL: "ftp://example.com"},
		{ViewportWidth: 100},
		{ViewportHeight: 100},
		{ActionTimeout: time.Millisecond},
		{GuardWorkers: 2048},
	}
	for _, candidate := range invalid {
		if err := applyChromeDefaults(&candidate); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("applyChromeDefaults(%#v) error = %v", candidate, err)
		}
	}
}

func TestChromeAllocatorConstruction(t *testing.T) {
	t.Parallel()

	ctx, cancel := chromeAllocator(context.Background(), ChromeConfig{ViewportWidth: 800, ViewportHeight: 600})
	if ctx == nil || cancel == nil {
		t.Fatal("local chromeAllocator() returned nil")
	}
	cancel()
	ctx, cancel = chromeAllocator(context.Background(), ChromeConfig{RemoteURL: "ws://127.0.0.1:9222"})
	if ctx == nil || cancel == nil {
		t.Fatal("remote chromeAllocator() returned nil")
	}
	cancel()
}

func TestChromeRunnerActionsAndClose(t *testing.T) {
	t.Parallel()

	root, rootCancel := context.WithCancel(context.Background())
	allocator, allocatorCancel := context.WithCancel(context.Background())
	_ = allocator
	actionCalls := 0
	runner := &ChromeRunner{
		ctx: root, cancel: rootCancel, allocatorCancel: allocatorCancel,
		actionTimeout: time.Second,
		runActions: func(_ context.Context, actions ...chromedp.Action) error {
			actionCalls += len(actions)
			return nil
		},
	}
	if err := runner.Navigate(context.Background(), "https://example.com"); err != nil {
		t.Fatalf("Navigate(): %v", err)
	}
	if err := runner.Evaluate(context.Background(), "1+1", nil); err != nil {
		t.Fatalf("Evaluate(): %v", err)
	}
	if err := runner.Click(context.Background(), "button"); err != nil {
		t.Fatalf("Click(): %v", err)
	}
	if err := runner.Type(context.Background(), "input", "hello", false); err != nil {
		t.Fatalf("Type(): %v", err)
	}
	if err := runner.Type(context.Background(), "input", "hello", true); err != nil {
		t.Fatalf("Type(submit): %v", err)
	}
	if image, err := runner.Screenshot(context.Background(), false); err != nil || len(image) != 0 {
		t.Fatalf("Screenshot() = %v, %v", image, err)
	}
	if image, err := runner.Screenshot(context.Background(), true); err != nil || len(image) != 0 {
		t.Fatalf("Screenshot(full) = %v, %v", image, err)
	}
	if actionCalls != 12 {
		t.Fatalf("action calls = %d, want 12", actionCalls)
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("Close(second): %v", err)
	}
	if root.Err() == nil {
		t.Fatal("Close() did not cancel browser context")
	}
	var nilRunner *ChromeRunner
	if err := nilRunner.Close(); err != nil {
		t.Fatalf("nil Close(): %v", err)
	}
	if err := nilRunner.run(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil run() error = %v", err)
	}
}

func TestChromeRunnerPropagatesCallerCancellationAndErrors(t *testing.T) {
	t.Parallel()

	runError := errors.New("CDP failed")
	runner := &ChromeRunner{
		ctx: context.Background(), cancel: func() {}, allocatorCancel: func() {},
		actionTimeout: time.Second,
		runActions:    func(context.Context, ...chromedp.Action) error { return runError },
	}
	if err := runner.Navigate(context.Background(), "https://example.com"); !errors.Is(err, runError) {
		t.Fatalf("Navigate(error) = %v", err)
	}
	runner.runActions = func(ctx context.Context, _ ...chromedp.Action) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runner.Evaluate(ctx, "1", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Evaluate(cancelled) error = %v", err)
	}
}

func TestChromeRunnerGuardsEveryRequest(t *testing.T) {
	t.Parallel()

	guard, _ := NewURLGuard(URLGuardConfig{Resolver: fakeResolver{addresses: map[string][]netip.Addr{
		"public.test":  {netip.MustParseAddr("93.184.216.34")},
		"private.test": {netip.MustParseAddr("127.0.0.1")},
	}}})
	continued := make(chan fetch.RequestID, 2)
	rejected := make(chan fetch.RequestID, 2)
	var listener func(any)
	runner := &ChromeRunner{
		ctx: context.Background(), guard: guard, actionTimeout: time.Second,
		guardSlots:   make(chan struct{}, 1),
		listenTarget: func(_ context.Context, callback func(any)) { listener = callback },
		continueRequest: func(_ context.Context, requestID fetch.RequestID) error {
			continued <- requestID
			return nil
		},
		rejectRequest: func(_ context.Context, requestID fetch.RequestID) error {
			rejected <- requestID
			return nil
		},
	}
	runner.listenForRequests()
	listener("ignored")
	listener(&fetch.EventRequestPaused{})

	public := &fetch.EventRequestPaused{
		RequestID: "public", Request: &network.Request{URL: "https://public.test"},
	}
	runner.guardSlots <- struct{}{}
	runner.handleRequest(public)
	if requestID := <-continued; requestID != "public" {
		t.Fatalf("continued request = %q", requestID)
	}
	private := &fetch.EventRequestPaused{
		RequestID: "private", Request: &network.Request{URL: "http://private.test"},
	}
	runner.guardSlots <- struct{}{}
	runner.handleRequest(private)
	if requestID := <-rejected; requestID != "private" {
		t.Fatalf("rejected request = %q", requestID)
	}

	runner.guardSlots <- struct{}{}
	listener(&fetch.EventRequestPaused{
		RequestID: "overloaded", Request: &network.Request{URL: "https://public.test"},
	})
	if requestID := <-rejected; requestID != "overloaded" {
		t.Fatalf("overload rejection = %q", requestID)
	}
	<-runner.guardSlots
}
