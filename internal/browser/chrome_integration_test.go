package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestChromeIntegration(t *testing.T) {
	if os.Getenv("GOFER_BROWSER_INTEGRATION") != "1" {
		t.Skip("set GOFER_BROWSER_INTEGRATION=1 to run with a local Chrome installation")
	}
	executable := findChromeExecutable()
	if executable == "" {
		t.Skip("Chrome executable not found")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Start</title><body>
<button onclick="document.title='Clicked'">Click me</button>
<input aria-label="Message">
</body>`))
	}))
	defer server.Close()
	guard, err := NewURLGuard(URLGuardConfig{AllowPrivateAddresses: true})
	if err != nil {
		t.Fatalf("NewURLGuard(): %v", err)
	}
	factory, err := NewChromeFactory(ChromeConfig{
		ExecutablePath: executable, ActionTimeout: 10 * time.Second,
	}, guard)
	if err != nil {
		t.Fatalf("NewChromeFactory(): %v", err)
	}
	session, err := factory(context.Background(), "integration")
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}
	defer func() { _ = session.Close() }()
	snapshot, err := session.Navigate(context.Background(), server.URL)
	if err != nil || snapshot.Title != "Start" || len(snapshot.Elements) != 2 {
		t.Fatalf("Navigate() = %#v, %v", snapshot, err)
	}
	snapshot, err = session.Click(context.Background(), 1)
	if err != nil || snapshot.Title != "Clicked" {
		t.Fatalf("Click() = %#v, %v", snapshot, err)
	}
	if _, err := session.Type(context.Background(), 2, "hello", false); err != nil {
		t.Fatalf("Type(): %v", err)
	}
	image, err := session.Screenshot(context.Background(), false)
	if err != nil || len(image) < 8 || string(image[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("Screenshot() returned %d bytes, %v", len(image), err)
	}
}

func findChromeExecutable() string {
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if executable, err := exec.LookPath(name); err == nil {
			return executable
		}
	}
	if runtime.GOOS == "darwin" {
		candidate := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}
