package browser

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// ChromeConfig configures a local Chrome process or remote CDP connection.
type ChromeConfig struct {
	ExecutablePath string
	RemoteURL      string
	Headful        bool
	ViewportWidth  int
	ViewportHeight int
	ActionTimeout  time.Duration
	GuardWorkers   int
}

// ChromeRunner drives Chrome through the DevTools Protocol.
type ChromeRunner struct {
	ctx             context.Context
	cancel          context.CancelFunc
	allocatorCancel context.CancelFunc
	guard           *URLGuard
	actionTimeout   time.Duration
	guardSlots      chan struct{}
	runActions      func(context.Context, ...chromedp.Action) error
	listenTarget    func(context.Context, func(any))
	continueRequest func(context.Context, fetch.RequestID) error
	rejectRequest   func(context.Context, fetch.RequestID) error
	closeOnce       sync.Once
}

// NewChromeFactory validates config and returns a Manager-compatible factory.
func NewChromeFactory(config ChromeConfig, guard *URLGuard) (Factory, error) {
	if guard == nil {
		return nil, fmt.Errorf("%w: URL guard is required", ErrInvalidConfig)
	}
	if err := applyChromeDefaults(&config); err != nil {
		return nil, err
	}
	return func(ctx context.Context, _ string) (Session, error) {
		runner, err := newChromeRunner(ctx, config, guard)
		if err != nil {
			return nil, err
		}
		automation, err := NewAutomation(runner, guard)
		if err != nil {
			_ = runner.Close()
			return nil, err
		}
		return automation, nil
	}, nil
}

func applyChromeDefaults(config *ChromeConfig) error {
	if err := validateChromeConnection(*config); err != nil {
		return err
	}
	applyChromeLimitDefaults(config)
	return validateChromeLimits(*config)
}

func validateChromeConnection(config ChromeConfig) error {
	if strings.IndexByte(config.ExecutablePath, 0) >= 0 {
		return fmt.Errorf("%w: Chrome executable contains NUL", ErrInvalidConfig)
	}
	if config.RemoteURL != "" {
		parsed, err := url.Parse(config.RemoteURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https" &&
			parsed.Scheme != "ws" && parsed.Scheme != "wss") {
			return fmt.Errorf("%w: remote CDP URL must be absolute HTTP(S) or WS(S)", ErrInvalidConfig)
		}
	}
	return nil
}

func applyChromeLimitDefaults(config *ChromeConfig) {
	if config.ViewportWidth == 0 {
		config.ViewportWidth = 1280
	}
	if config.ViewportHeight == 0 {
		config.ViewportHeight = 720
	}
	if config.ActionTimeout == 0 {
		config.ActionTimeout = 30 * time.Second
	}
	if config.GuardWorkers == 0 {
		config.GuardWorkers = 64
	}
}

func validateChromeLimits(config ChromeConfig) error {
	if config.ViewportWidth < 320 || config.ViewportWidth > 7680 ||
		config.ViewportHeight < 200 || config.ViewportHeight > 4320 ||
		config.ActionTimeout < time.Second || config.ActionTimeout > 10*time.Minute ||
		config.GuardWorkers < 1 || config.GuardWorkers > 1024 {
		return fmt.Errorf("%w: invalid Chrome viewport, timeout, or guard worker count", ErrInvalidConfig)
	}
	return nil
}

func newChromeRunner(parent context.Context, config ChromeConfig, guard *URLGuard) (*ChromeRunner, error) {
	allocatorContext, allocatorCancel := chromeAllocator(parent, config)
	browserContext, browserCancel := chromedp.NewContext(allocatorContext)
	runner := &ChromeRunner{
		ctx: browserContext, cancel: browserCancel, allocatorCancel: allocatorCancel,
		guard: guard, actionTimeout: config.ActionTimeout, guardSlots: make(chan struct{}, config.GuardWorkers),
		runActions: chromedp.Run, listenTarget: chromedp.ListenTarget,
		continueRequest: func(ctx context.Context, requestID fetch.RequestID) error {
			return chromedp.Run(ctx, fetch.ContinueRequest(requestID))
		},
		rejectRequest: func(ctx context.Context, requestID fetch.RequestID) error {
			return chromedp.Run(ctx, fetch.FailRequest(requestID, network.ErrorReasonBlockedByClient))
		},
	}
	runner.listenForRequests()
	// The first Run owns the browser process lifetime, so it must use the
	// long-lived session context rather than a per-action child context.
	if err := runner.runActions(runner.ctx, fetch.Enable()); err != nil {
		_ = runner.Close()
		return nil, fmt.Errorf("start Chrome browser: %w", err)
	}
	return runner, nil
}

func chromeAllocator(parent context.Context, config ChromeConfig) (context.Context, context.CancelFunc) {
	if config.RemoteURL != "" {
		return chromedp.NewRemoteAllocator(parent, config.RemoteURL)
	}
	options := append([]chromedp.ExecAllocatorOption(nil), chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options,
		chromedp.Flag("headless", !config.Headful),
		chromedp.Flag("disable-gpu", true),
		chromedp.WindowSize(config.ViewportWidth, config.ViewportHeight),
	)
	if config.ExecutablePath != "" {
		options = append(options, chromedp.ExecPath(config.ExecutablePath))
	}
	return chromedp.NewExecAllocator(parent, options...)
}

// Navigate opens a URL.
func (runner *ChromeRunner) Navigate(ctx context.Context, rawURL string) error {
	return runner.run(ctx, chromedp.Navigate(rawURL), chromedp.WaitReady("body", chromedp.ByQuery))
}

// Evaluate executes trusted Gofer-owned JavaScript.
func (runner *ChromeRunner) Evaluate(ctx context.Context, expression string, output any) error {
	return runner.run(ctx, chromedp.Evaluate(expression, output))
}

// Click clicks one trusted selector and waits for a usable document.
func (runner *ChromeRunner) Click(ctx context.Context, selector string) error {
	return runner.run(ctx,
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
}

// Type fills one trusted selector and optionally presses Enter.
func (runner *ChromeRunner) Type(ctx context.Context, selector, text string, submit bool) error {
	actions := []chromedp.Action{
		chromedp.Focus(selector, chromedp.ByQuery),
		chromedp.SetValue(selector, text, chromedp.ByQuery),
	}
	if submit {
		actions = append(actions, chromedp.SendKeys(selector, kb.Enter, chromedp.ByQuery))
	}
	return runner.run(ctx, actions...)
}

// Screenshot captures the current viewport or full page as PNG.
func (runner *ChromeRunner) Screenshot(ctx context.Context, fullPage bool) ([]byte, error) {
	var image []byte
	if fullPage {
		if err := runner.run(ctx, chromedp.FullScreenshot(&image, 100)); err != nil {
			return nil, err
		}
	} else {
		if err := runner.run(ctx, chromedp.CaptureScreenshot(&image)); err != nil {
			return nil, err
		}
	}
	return append([]byte(nil), image...), nil
}

func (runner *ChromeRunner) run(ctx context.Context, actions ...chromedp.Action) error {
	if runner == nil || runner.ctx == nil || runner.runActions == nil {
		return fmt.Errorf("%w: Chrome runner is not configured", ErrInvalidConfig)
	}
	operationContext, cancel := runner.operationContext(ctx)
	defer cancel()
	return runner.runActions(operationContext, actions...)
}

func (runner *ChromeRunner) operationContext(caller context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(runner.ctx, runner.actionTimeout)
	if caller == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(caller, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (runner *ChromeRunner) listenForRequests() {
	runner.listenTarget(runner.ctx, func(event any) {
		paused, ok := event.(*fetch.EventRequestPaused)
		if !ok || paused.Request == nil {
			return
		}
		select {
		case runner.guardSlots <- struct{}{}:
			go runner.handleRequest(paused)
		default:
			go runner.failRequest(paused.RequestID)
		}
	})
}

func (runner *ChromeRunner) handleRequest(paused *fetch.EventRequestPaused) {
	defer func() { <-runner.guardSlots }()
	ctx, cancel := context.WithTimeout(runner.ctx, runner.actionTimeout)
	defer cancel()
	if err := runner.guard.ValidateRequest(ctx, paused.Request.URL); err != nil {
		_ = runner.rejectRequest(ctx, paused.RequestID)
		return
	}
	_ = runner.continueRequest(ctx, paused.RequestID)
}

func (runner *ChromeRunner) failRequest(requestID fetch.RequestID) {
	ctx, cancel := context.WithTimeout(runner.ctx, runner.actionTimeout)
	defer cancel()
	_ = runner.rejectRequest(ctx, requestID)
}

// Close idempotently closes the target and allocator contexts.
func (runner *ChromeRunner) Close() error {
	if runner == nil {
		return nil
	}
	runner.closeOnce.Do(func() {
		runner.cancel()
		runner.allocatorCancel()
	})
	return nil
}

var _ Runner = (*ChromeRunner)(nil)
