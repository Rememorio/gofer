package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const githubTestSecret = "github-test-secret-at-least-24-bytes"

func TestGitHubVerifiesFiltersAndQueues(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	dispatched := make(chan Request, 1)
	manager := githubTestManager(t, now, dispatcherFunc(func(_ context.Context, request Request) (Reply, error) {
		dispatched <- request
		return Reply{Text: "recorded only"}, nil
	}))
	requireMention := true
	provider := githubTestProvider(t, manager, now, GitHubSubscription{
		ID: "maintainer", Repository: "Rememorio/gofer", AssistantID: "reviewer", BotLogin: "gofer-bot",
		Triggers: map[string]GitHubTrigger{"issue_comment": {RequireMention: &requireMention}},
	})
	startGitHubManager(t, manager, provider)
	body := []byte(`{"action":"created","repository":{"full_name":"Rememorio/gofer"},"issue":{"number":42,"title":"Race","body":"context"},"comment":{"body":"please @gofer-bot inspect this","user":{"login":"alice"}},"sender":{"login":"alice"}}`)
	response := httptest.NewRecorder()
	provider.ServeHTTP(response, signedGitHubRequest(body, "issue_comment", "delivery-1"))
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"dispatched_subscriptions":["maintainer"]`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	select {
	case request := <-dispatched:
		if request.Identity.UserID != "owner" || request.Message.ID != "delivery-1:maintainer" || request.Message.TopicID != "42" ||
			request.Message.Metadata["assistant_id"] != "reviewer" || request.Message.Metadata["github_sender"] != "alice" ||
			!strings.Contains(request.Message.Text, "`gh issue comment`") {
			t.Fatalf("dispatch = %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("GitHub event was not dispatched")
	}
	duplicate := httptest.NewRecorder()
	provider.ServeHTTP(duplicate, signedGitHubRequest(body, "issue_comment", "delivery-1"))
	if duplicate.Code != http.StatusAccepted {
		t.Fatalf("duplicate enqueue = %d %s", duplicate.Code, duplicate.Body.String())
	}
	select {
	case request := <-dispatched:
		t.Fatalf("duplicate dispatch = %#v", request)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestGitHubRejectsInvalidRequestsAndHandlesNoOps(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	manager := githubTestManager(t, now, dispatcherFunc(func(context.Context, Request) (Reply, error) { return Reply{}, nil }))
	provider := githubTestProvider(t, manager, now, GitHubSubscription{
		ID: "maintainer", Repository: "Rememorio/gofer", Triggers: map[string]GitHubTrigger{"issues": {}},
	})
	startGitHubManager(t, manager, provider)
	valid := []byte(`{"action":"opened","repository":{"full_name":"Rememorio/gofer"},"issue":{"number":1}}`)
	tests := []struct {
		name   string
		make   func() *http.Request
		status int
	}{
		{name: "method", make: func() *http.Request {
			return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		}, status: http.StatusMethodNotAllowed},
		{name: "signature missing", make: func() *http.Request {
			request := signedGitHubRequest(valid, "issues", "one")
			request.Header.Del("X-Hub-Signature-256")
			return request
		}, status: http.StatusUnauthorized},
		{name: "signature malformed", make: func() *http.Request {
			request := signedGitHubRequest(valid, "issues", "one")
			request.Header.Set("X-Hub-Signature-256", "sha256=xyz")
			return request
		}, status: http.StatusUnauthorized},
		{name: "event missing", make: func() *http.Request { return signedGitHubRequest(valid, "", "one") }, status: http.StatusBadRequest},
		{name: "delivery missing", make: func() *http.Request { return signedGitHubRequest(valid, "issues", "") }, status: http.StatusBadRequest},
		{name: "invalid JSON", make: func() *http.Request { return signedGitHubRequest([]byte("{"), "issues", "one") }, status: http.StatusBadRequest},
		{name: "empty JSON", make: func() *http.Request { return signedGitHubRequest(nil, "issues", "one") }, status: http.StatusBadRequest},
		{name: "unknown event", make: func() *http.Request { return signedGitHubRequest(valid, "workflow_run", "one") }, status: http.StatusOK},
		{name: "no target", make: func() *http.Request { return signedGitHubRequest([]byte(`{"zen":"hello"}`), "ping", "one") }, status: http.StatusOK},
		{name: "too large", make: func() *http.Request { return signedGitHubRequest(bytes.Repeat([]byte("x"), 1025), "issues", "one") }, status: http.StatusRequestEntityTooLarge},
	}
	provider.maxBodyBytes = 1024
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			provider.ServeHTTP(response, test.make())
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestGitHubTriggerDefaultsMentionsAuthorsAndSelfEvents(t *testing.T) {
	t.Parallel()
	opened := githubPayload("opened", "please review", "alice")
	closed := githubPayload("closed", "please review", "alice")
	subscription := GitHubSubscription{ID: "agent", BotLogin: "gofer", DefaultMentionLogin: "default"}
	pullRequest := resolveGitHubTrigger("pull_request", GitHubTrigger{})
	if fire, _ := githubShouldFire("pull_request", opened, subscription, pullRequest); !fire {
		t.Fatal("default opened pull request did not fire")
	}
	if fire, reason := githubShouldFire("pull_request", closed, subscription, pullRequest); fire || reason != "action_not_allowed" {
		t.Fatalf("closed pull request = %v %q", fire, reason)
	}

	comment := resolveGitHubTrigger("issue_comment", GitHubTrigger{})
	if fire, reason := githubShouldFire("issue_comment", githubPayload("created", "hello @gofer-bot", "alice"), subscription, comment); fire || reason != "mention_required" {
		t.Fatalf("mention boundary = %v %q", fire, reason)
	}
	if fire, _ := githubShouldFire("issue_comment", githubPayload("created", "mail alice@gofer and @gofer", "alice"), subscription, comment); !fire {
		t.Fatal("valid mention did not fire")
	}
	comment.allowAuthors = []string{"ALICE"}
	if fire, reason := githubShouldFire("issue_comment", githubPayload("created", "no mention", "alice"), subscription, comment); !fire || reason != "allowed_author" {
		t.Fatalf("allowed author = %v %q", fire, reason)
	}
	if fire, reason := githubShouldFire("issue_comment", githubPayload("created", "@gofer", "gofer[bot]"), subscription, comment); fire || reason != "self_event" {
		t.Fatalf("self event = %v %q", fire, reason)
	}
}

func TestGitHubReviewNoiseSuppressionIsPerSubscription(t *testing.T) {
	t.Parallel()
	payload := githubPayload("created", "@gofer please fix", "alice")
	payload["comment"].(map[string]any)["pull_request_review_id"] = float64(7)
	subscription := GitHubSubscription{Triggers: map[string]GitHubTrigger{
		"pull_request_review": {}, "pull_request_review_comment": {},
	}}
	if !redundantGitHubReviewComment("pull_request_review_comment", payload, subscription) {
		t.Fatal("companion review comment was not identified")
	}
	requireMention := true
	subscription.Triggers["pull_request_review"] = GitHubTrigger{RequireMention: &requireMention}
	if redundantGitHubReviewComment("pull_request_review_comment", payload, subscription) {
		t.Fatal("mention-gated review was treated as guaranteed coverage")
	}
	payload["comment"].(map[string]any)["in_reply_to_id"] = float64(8)
	if redundantGitHubReviewComment("pull_request_review_comment", payload, subscription) {
		t.Fatal("thread reply was treated as review fan-out")
	}
}

func TestGitHubBackpressureAndUnverifiedMode(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	started, release := make(chan struct{}), make(chan struct{})
	manager := githubTestManager(t, now, dispatcherFunc(func(ctx context.Context, _ Request) (Reply, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-ctx.Done():
			return Reply{}, ctx.Err()
		case <-release:
			return Reply{Text: "done"}, nil
		}
	}))
	provider := githubTestProvider(t, manager, now, GitHubSubscription{ID: "maintainer", Repository: "Rememorio/gofer", Triggers: map[string]GitHubTrigger{"issues": {}}})
	startGitHubManager(t, manager, provider)
	body := []byte(`{"action":"opened","repository":{"full_name":"Rememorio/gofer"},"issue":{"number":1}}`)
	first := httptest.NewRecorder()
	provider.ServeHTTP(first, signedGitHubRequest(body, "issues", "one"))
	<-started
	second := httptest.NewRecorder()
	provider.ServeHTTP(second, signedGitHubRequest(body, "issues", "two"))
	third := httptest.NewRecorder()
	provider.ServeHTTP(third, signedGitHubRequest(body, "issues", "three"))
	if second.Code != http.StatusAccepted || third.Code != http.StatusServiceUnavailable || third.Header().Get("Retry-After") != "1" {
		t.Fatalf("backpressure = second %d, third %d/%q", second.Code, third.Code, third.Header().Get("Retry-After"))
	}
	close(release)

	unverified, err := NewGitHub(GitHubConfig{Manager: manager, AllowUnverified: true, MaxBodyBytes: 1024, Now: func() time.Time { return now }, Subscriptions: []GitHubSubscription{{ID: "dev", Repository: "Rememorio/gofer", Triggers: map[string]GitHubTrigger{"issues": {}}}}})
	if err != nil || !unverified.verify(body, "") {
		t.Fatalf("unverified mode = %v", err)
	}
}

func TestGitHubValidationSendAndPromptBranches(t *testing.T) {
	t.Parallel()
	if _, err := NewGitHub(GitHubConfig{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewGitHub() = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&GitHub{}).Send(ctx, Reply{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() = %v", err)
	}
	if err := (&GitHub{}).Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	for _, event := range []string{"issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "ping"} {
		prompt := githubPrompt(event, githubPayload("opened", "body", "alice"))
		if strings.TrimSpace(prompt) == "" {
			t.Fatalf("empty prompt for %s", event)
		}
	}
	if githubMentioned("prefix @gofer-bot suffix", "gofer") || !githubMentioned("prefix @GOFER suffix", "gofer") {
		t.Fatal("mention matching boundaries or casing failed")
	}
}

func githubTestManager(t *testing.T, now time.Time, dispatcher Dispatcher) *Manager {
	t.Helper()
	state := NewMemoryState()
	binding, err := NewBinding("owner", githubProvider, "Rememorio/gofer", "maintainer", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{Resolver: state, Dispatcher: dispatcher, Dedupe: state, MaxInflight: 1, QueueCapacity: 1, DedupeTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func githubTestProvider(t *testing.T, manager *Manager, now time.Time, subscription GitHubSubscription) *GitHub {
	t.Helper()
	provider, err := NewGitHub(GitHubConfig{Manager: manager, Secret: githubTestSecret, MaxBodyBytes: 1 << 20, Subscriptions: []GitHubSubscription{subscription}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func startGitHubManager(t *testing.T, manager *Manager, provider *GitHub) {
	t.Helper()
	if err := manager.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
}

func signedGitHubRequest(body []byte, event, delivery string) *http.Request {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/webhooks/github", bytes.NewReader(body))
	digest := hmac.New(sha256.New, []byte(githubTestSecret))
	_, _ = digest.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(digest.Sum(nil)))
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-GitHub-Delivery", delivery)
	return request
}

func githubPayload(action, body, author string) map[string]any {
	raw := []byte(`{"action":"` + action + `","repository":{"full_name":"Rememorio/gofer"},"issue":{"number":7,"title":"Issue","body":"` + body + `","user":{"login":"` + author + `"}},"pull_request":{"number":7,"title":"PR","body":"` + body + `","user":{"login":"` + author + `"}},"comment":{"body":"` + body + `","user":{"login":"` + author + `"}},"review":{"body":"` + body + `","user":{"login":"` + author + `"}},"sender":{"login":"` + author + `"}}`)
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	return payload
}
