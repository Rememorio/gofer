package channel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const githubProvider = "github"

var githubKnownEvents = map[string]struct{}{
	"ping": {}, "issues": {}, "issue_comment": {}, "pull_request": {},
	"pull_request_review": {}, "pull_request_review_comment": {},
}

// GitHubTrigger filters one explicitly subscribed webhook event.
type GitHubTrigger struct {
	Actions        []string
	RequireMention *bool
	MentionLogin   string
	AllowAuthors   []string
}

// GitHubSubscription routes repository events to one pre-bound Gofer identity.
type GitHubSubscription struct {
	ID                  string
	Repository          string
	AssistantID         string
	BotLogin            string
	DefaultMentionLogin string
	Triggers            map[string]GitHubTrigger
}

// GitHubConfig controls fail-closed GitHub webhook ingress.
type GitHubConfig struct {
	Manager         *Manager
	Secret          string
	AllowUnverified bool
	MaxBodyBytes    int64
	Subscriptions   []GitHubSubscription
	Now             func() time.Time
}

// GitHub accepts verified repository webhooks. Its outbound path is
// intentionally log-only: the agent uses gh during its run for GitHub writes.
type GitHub struct {
	manager         *Manager
	secret          []byte
	allowUnverified bool
	maxBodyBytes    int64
	subscriptions   map[string][]GitHubSubscription
	now             func() time.Time
}

// NewGitHub validates and constructs a GitHub webhook adapter.
func NewGitHub(config GitHubConfig) (*GitHub, error) {
	if config.Manager == nil || config.MaxBodyBytes < 1024 || config.MaxBodyBytes > 16<<20 ||
		len(config.Secret) < 24 && !config.AllowUnverified || len(config.Subscriptions) == 0 {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	provider := &GitHub{
		manager: config.Manager, secret: []byte(config.Secret), allowUnverified: config.AllowUnverified,
		maxBodyBytes: config.MaxBodyBytes, subscriptions: make(map[string][]GitHubSubscription), now: config.Now,
	}
	for _, subscription := range config.Subscriptions {
		if err := validateGitHubSubscription(subscription); err != nil {
			return nil, err
		}
		repository := strings.ToLower(subscription.Repository)
		provider.subscriptions[repository] = append(provider.subscriptions[repository], cloneGitHubSubscription(subscription))
	}
	return provider, nil
}

// Name returns the provider registry name.
func (*GitHub) Name() string { return githubProvider }

// Send completes the channel contract without posting the final run message.
// GitHub prompts explicitly direct agents to use gh for visible side effects.
func (*GitHub) Send(ctx context.Context, _ Reply) error {
	if ctx == nil {
		return ErrInvalid
	}
	return ctx.Err()
}

// Close releases no resources and is safe to call repeatedly.
func (*GitHub) Close() error { return nil }

// ServeHTTP verifies, filters, and enqueues one GitHub webhook delivery.
func (provider *GitHub) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeGitHubError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := provider.readBody(writer, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeGitHubError(writer, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeGitHubError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if !provider.verify(body, request.Header.Get("X-Hub-Signature-256")) {
		writeGitHubError(writer, http.StatusUnauthorized, "invalid or missing GitHub signature")
		return
	}
	event, delivery := strings.TrimSpace(request.Header.Get("X-GitHub-Event")), strings.TrimSpace(request.Header.Get("X-GitHub-Delivery"))
	if event == "" || delivery == "" || len(event) > 128 || len(delivery) > 256 {
		writeGitHubError(writer, http.StatusBadRequest, "missing or invalid GitHub event headers")
		return
	}
	var payload map[string]any
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		writeGitHubError(writer, http.StatusBadRequest, "invalid JSON body")
		return
	}
	result, status, err := provider.dispatch(request.Context(), event, delivery, payload)
	if err != nil {
		if errors.Is(err, ErrBusy) || errors.Is(err, ErrClosed) {
			writer.Header().Set("Retry-After", "1")
			writeGitHubError(writer, http.StatusServiceUnavailable, "channel ingress unavailable")
			return
		}
		writeGitHubError(writer, http.StatusInternalServerError, "GitHub webhook dispatch failed")
		return
	}
	writeGitHubJSON(writer, status, result)
}

func (provider *GitHub) readBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	request.Body = http.MaxBytesReader(writer, request.Body, provider.maxBodyBytes)
	data, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err = request.Body.Close(); err != nil {
		return nil, err
	}
	return data, nil
}

func (provider *GitHub) verify(body []byte, signature string) bool {
	if len(provider.secret) == 0 {
		return provider.allowUnverified
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimSpace(strings.TrimPrefix(signature, prefix)))
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	digest := hmac.New(sha256.New, provider.secret)
	_, _ = digest.Write(body)
	return hmac.Equal(provided, digest.Sum(nil))
}

type githubDispatchResult struct {
	OK         bool             `json:"ok"`
	Event      string           `json:"event"`
	Delivery   string           `json:"delivery"`
	Handled    bool             `json:"handled"`
	Matched    []string         `json:"matched_subscriptions"`
	Dispatched []string         `json:"dispatched_subscriptions"`
	Skipped    []githubSkipItem `json:"skipped"`
}

type githubSkipItem struct {
	Subscription string `json:"subscription,omitempty"`
	Reason       string `json:"reason"`
}

func (provider *GitHub) dispatch(ctx context.Context, event, delivery string, payload map[string]any) (githubDispatchResult, int, error) {
	result := githubDispatchResult{OK: true, Event: event, Delivery: delivery}
	if _, known := githubKnownEvents[event]; !known {
		return result, http.StatusOK, nil
	}
	result.Handled = true
	repository, number, ok := githubTarget(event, payload)
	if !ok {
		result.Skipped = append(result.Skipped, githubSkipItem{Reason: "no_target"})
		return result, http.StatusOK, nil
	}
	for _, subscription := range provider.subscriptions[strings.ToLower(repository)] {
		trigger, subscribed := subscription.Triggers[event]
		if !subscribed {
			continue
		}
		result.Matched = append(result.Matched, subscription.ID)
		fire, reason := githubShouldFire(event, payload, subscription, resolveGitHubTrigger(event, trigger))
		if fire && redundantGitHubReviewComment(event, payload, subscription) {
			fire, reason = false, "redundant_review_comment"
		}
		if !fire {
			result.Skipped = append(result.Skipped, githubSkipItem{Subscription: subscription.ID, Reason: reason})
			continue
		}
		message := provider.message(subscription, event, delivery, repository, number, payload)
		if err := provider.manager.Submit(ctx, message); err != nil {
			return result, http.StatusServiceUnavailable, err
		}
		result.Dispatched = append(result.Dispatched, subscription.ID)
	}
	if len(result.Dispatched) == 0 {
		return result, http.StatusOK, nil
	}
	return result, http.StatusAccepted, nil
}

func (provider *GitHub) message(subscription GitHubSubscription, event, delivery, repository string, number int64, payload map[string]any) Message {
	metadata := map[string]string{
		"github_event": event, "github_delivery": delivery, "github_repository": repository,
		"github_number": strconv.FormatInt(number, 10), "github_sender": truncateGitHub(githubString(payload, "sender", "login"), 256),
	}
	if subscription.AssistantID != "" {
		metadata["assistant_id"] = subscription.AssistantID
	}
	return Message{
		ID: delivery + ":" + subscription.ID, Provider: githubProvider, WorkspaceID: subscription.Repository,
		ExternalUserID: subscription.ID, ChatID: repository, TopicID: strconv.FormatInt(number, 10),
		Text: githubPrompt(event, payload), Metadata: metadata, ReceivedAt: provider.now().UTC(),
	}
}

func validateGitHubSubscription(subscription GitHubSubscription) error {
	if strings.TrimSpace(subscription.ID) == "" || subscription.ID != strings.TrimSpace(subscription.ID) || len(subscription.ID) > 128 ||
		strings.TrimSpace(subscription.Repository) == "" || subscription.Repository != strings.TrimSpace(subscription.Repository) || len(subscription.Repository) > 200 ||
		len(subscription.AssistantID) > 128 || len(subscription.BotLogin) > 39 || len(subscription.DefaultMentionLogin) > 39 || len(subscription.Triggers) == 0 {
		return ErrInvalid
	}
	for event := range subscription.Triggers {
		if _, known := githubKnownEvents[event]; !known {
			return ErrInvalid
		}
	}
	return nil
}

func cloneGitHubSubscription(subscription GitHubSubscription) GitHubSubscription {
	cloned := subscription
	cloned.Triggers = make(map[string]GitHubTrigger, len(subscription.Triggers))
	for event, trigger := range subscription.Triggers {
		trigger.Actions = append([]string(nil), trigger.Actions...)
		trigger.AllowAuthors = append([]string(nil), trigger.AllowAuthors...)
		if trigger.RequireMention != nil {
			value := *trigger.RequireMention
			trigger.RequireMention = &value
		}
		cloned.Triggers[event] = trigger
	}
	return cloned
}

type resolvedGitHubTrigger struct {
	actions        []string
	requireMention bool
	mentionLogin   string
	allowAuthors   []string
}

func resolveGitHubTrigger(event string, trigger GitHubTrigger) resolvedGitHubTrigger {
	resolved := resolvedGitHubTrigger{
		actions: append([]string(nil), trigger.Actions...), mentionLogin: trigger.MentionLogin,
		allowAuthors: append([]string(nil), trigger.AllowAuthors...),
	}
	if trigger.Actions == nil && event == "pull_request" {
		resolved.actions = []string{"opened"}
	}
	if trigger.RequireMention != nil {
		resolved.requireMention = *trigger.RequireMention
	} else {
		resolved.requireMention = event == "issue_comment" || event == "pull_request_review_comment"
	}
	return resolved
}

func githubShouldFire(event string, payload map[string]any, subscription GitHubSubscription, trigger resolvedGitHubTrigger) (bool, string) {
	if githubSelfEvent(payload, subscription, trigger) {
		return false, "self_event"
	}
	action := githubString(payload, "action")
	if trigger.actions != nil && !containsExact(trigger.actions, action) {
		return false, "action_not_allowed"
	}
	author := githubAuthor(event, payload)
	if containsFold(trigger.allowAuthors, author) {
		return true, "allowed_author"
	}
	if trigger.requireMention {
		login := firstNonEmpty(trigger.mentionLogin, subscription.BotLogin, subscription.DefaultMentionLogin)
		if login == "" || !githubMentioned(githubBody(event, payload), login) {
			return false, "mention_required"
		}
	}
	if action != "" {
		return true, "action=" + action
	}
	return true, "ok"
}

func githubSelfEvent(payload map[string]any, subscription GitHubSubscription, trigger resolvedGitHubTrigger) bool {
	sender := trimBotSuffix(githubString(payload, "sender", "login"))
	if sender == "" {
		return false
	}
	identities := []string{subscription.BotLogin, trigger.mentionLogin, subscription.DefaultMentionLogin}
	for _, identity := range identities {
		if identity != "" && strings.EqualFold(sender, identity) {
			return true
		}
	}
	return false
}

func redundantGitHubReviewComment(event string, payload map[string]any, subscription GitHubSubscription) bool {
	if event != "pull_request_review_comment" || githubValue(payload, "comment", "pull_request_review_id") == nil ||
		githubValue(payload, "comment", "in_reply_to_id") != nil {
		return false
	}
	review, subscribed := subscription.Triggers["pull_request_review"]
	return subscribed && !resolveGitHubTrigger("pull_request_review", review).requireMention
}

func githubTarget(event string, payload map[string]any) (string, int64, bool) {
	repository := githubString(payload, "repository", "full_name")
	var value any
	switch event {
	case "issues", "issue_comment":
		value = githubValue(payload, "issue", "number")
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		value = githubValue(payload, "pull_request", "number")
		if value == nil {
			value = payload["number"]
		}
	default:
		return "", 0, false
	}
	number, ok := githubNumber(value)
	return repository, number, repository != "" && ok && number > 0
}

func githubNumber(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func githubAuthor(event string, payload map[string]any) string {
	switch event {
	case "issue_comment", "pull_request_review_comment":
		return githubString(payload, "comment", "user", "login")
	case "issues":
		return githubString(payload, "issue", "user", "login")
	case "pull_request":
		return githubString(payload, "pull_request", "user", "login")
	case "pull_request_review":
		return githubString(payload, "review", "user", "login")
	default:
		return githubString(payload, "sender", "login")
	}
}

func githubBody(event string, payload map[string]any) string {
	switch event {
	case "issue_comment", "pull_request_review_comment":
		return githubString(payload, "comment", "body")
	case "issues":
		return githubString(payload, "issue", "body")
	case "pull_request":
		return githubString(payload, "pull_request", "body")
	case "pull_request_review":
		return githubString(payload, "review", "body")
	default:
		return ""
	}
}

func githubMentioned(body, login string) bool {
	bodyLower, loginLower := strings.ToLower(body), strings.ToLower(login)
	needle := "@" + loginLower
	for offset := 0; offset < len(bodyLower); {
		index := strings.Index(bodyLower[offset:], needle)
		if index < 0 {
			return false
		}
		index += offset
		beforeOK := index == 0 || !githubLoginByte(bodyLower[index-1])
		after := index + len(needle)
		afterOK := after == len(bodyLower) || !githubLoginByte(bodyLower[after])
		if beforeOK && afterOK {
			return true
		}
		offset = index + len(needle)
	}
	return false
}

func githubLoginByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '-'
}

func githubPrompt(event string, payload map[string]any) string {
	repository := firstNonEmpty(githubString(payload, "repository", "full_name"), "(unknown repository)")
	switch event {
	case "issues":
		return githubIssuePrompt(repository, payload)
	case "issue_comment":
		return githubIssueCommentPrompt(repository, payload)
	case "pull_request":
		return githubPullRequestPrompt(repository, payload)
	case "pull_request_review":
		return githubReviewPrompt(repository, payload)
	case "pull_request_review_comment":
		return githubReviewCommentPrompt(repository, payload)
	default:
		return fmt.Sprintf("GitHub event %q fired on %s. No action is required.", event, repository)
	}
}

func githubIssuePrompt(repository string, payload map[string]any) string {
	return fmt.Sprintf("An issue was %s on %s:\n\n#%v %s\nAuthor: %s\nURL: %s\n\nDescription:\n%s\n\nDecide what action, if any, to take and carry it out. The final assistant message is only recorded in the Gofer run log. Use `gh issue comment` or `gh pr create` during the run for any visible GitHub response.",
		firstNonEmpty(githubString(payload, "action"), "opened"), repository, githubValue(payload, "issue", "number"),
		firstNonEmpty(githubString(payload, "issue", "title"), "(no title)"), firstNonEmpty(githubString(payload, "issue", "user", "login"), "(unknown)"),
		firstNonEmpty(githubString(payload, "issue", "html_url"), "(no URL)"), firstNonEmpty(truncateGitHub(githubString(payload, "issue", "body"), 4000), "(no description)"))
}

func githubIssueCommentPrompt(repository string, payload map[string]any) string {
	return fmt.Sprintf("A new comment was posted on issue or pull request #%v in %s.\n\nParent: %s\n%s\n\nComment by %s:\n%s\n\nDecide what action, if any, to take. The final assistant message is only recorded in the Gofer run log. Use `gh issue comment` during the run for any visible GitHub response.",
		githubValue(payload, "issue", "number"), repository, firstNonEmpty(githubString(payload, "issue", "title"), "(no title)"),
		firstNonEmpty(truncateGitHub(githubString(payload, "issue", "body"), 3000), "(no description)"),
		firstNonEmpty(githubString(payload, "comment", "user", "login"), "(unknown)"), firstNonEmpty(truncateGitHub(githubString(payload, "comment", "body"), 4000), "(empty comment)"))
}

func githubPullRequestPrompt(repository string, payload map[string]any) string {
	return fmt.Sprintf("A pull request was %s on %s.\n\n#%v %s\nAuthor: %s\nURL: %s\n\nDescription:\n%s\n\nDecide what action, if any, to take and carry it out. The final assistant message is only recorded in the Gofer run log. Use `gh pr comment` or `gh pr review` during the run for any visible GitHub response.",
		firstNonEmpty(githubString(payload, "action"), "opened"), repository, githubValue(payload, "pull_request", "number"),
		firstNonEmpty(githubString(payload, "pull_request", "title"), "(no title)"), firstNonEmpty(githubString(payload, "pull_request", "user", "login"), "(unknown)"),
		firstNonEmpty(githubString(payload, "pull_request", "html_url"), "(no URL)"), firstNonEmpty(truncateGitHub(githubString(payload, "pull_request", "body"), 4000), "(no description)"))
}

func githubReviewPrompt(repository string, payload map[string]any) string {
	return fmt.Sprintf("A pull request review was submitted on #%v in %s.\n\nReviewer: %s\nState: %s\nBody:\n%s\n\nFetch this review's inline comments with `gh api` if needed. Decide what action, if any, to take. The final assistant message is only recorded in the Gofer run log; use `gh` during the run for visible GitHub changes.",
		githubValue(payload, "pull_request", "number"), repository, firstNonEmpty(githubString(payload, "review", "user", "login"), "(unknown)"),
		firstNonEmpty(githubString(payload, "review", "state"), "(unknown)"), firstNonEmpty(truncateGitHub(githubString(payload, "review", "body"), 4000), "(no review body)"))
}

func githubReviewCommentPrompt(repository string, payload map[string]any) string {
	return fmt.Sprintf("A review comment was posted on pull request #%v in %s.\n\nAuthor: %s\nFile: %s:%v\nDiff context:\n%s\n\nComment:\n%s\n\nDecide what action, if any, to take. The final assistant message is only recorded in the Gofer run log; use `gh` during the run for visible GitHub changes.",
		githubValue(payload, "pull_request", "number"), repository, firstNonEmpty(githubString(payload, "comment", "user", "login"), "(unknown)"),
		firstNonEmpty(githubString(payload, "comment", "path"), "(unknown file)"), githubValue(payload, "comment", "line"),
		firstNonEmpty(truncateGitHub(githubString(payload, "comment", "diff_hunk"), 2000), "(no diff context)"), firstNonEmpty(truncateGitHub(githubString(payload, "comment", "body"), 4000), "(empty comment)"))
}

func githubValue(payload map[string]any, path ...string) any {
	var current any = payload
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func githubString(payload map[string]any, path ...string) string {
	value, _ := githubValue(payload, path...).(string)
	return value
}

func truncateGitHub(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit-16] + "\n[…truncated…]"
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func trimBotSuffix(login string) string {
	if strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return login[:len(login)-5]
	}
	return login
}

func writeGitHubError(writer http.ResponseWriter, status int, message string) {
	writeGitHubJSON(writer, status, map[string]any{"ok": false, "error": message})
}

func writeGitHubJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
