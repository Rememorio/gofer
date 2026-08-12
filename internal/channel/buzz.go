package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// BuzzProvider is the normalized Buzz/Nostr channel name.
	BuzzProvider                = "buzz"
	buzzEditMaxBytes            = 60_000
	buzzMaxFutureSkew           = time.Minute
	buzzMaxCachedChannels       = 512
	buzzMaxChannelSubscriptions = 256
	buzzMaxResubscribeAttempts  = 3
	buzzMembershipLookback      = time.Minute
	buzzDiscoverySubscription   = "buzz-discovery"
	buzzMembershipSubscription  = "buzz-membership"
	buzzChatSubscriptionPrefix  = "buzz-chat-"
)

var buzzPermanentClosePrefixes = []string{"auth-required:", "restricted:", "blocked:", "mute:", "invalid:", "pow:"}
var buzzPermanentCloseMarkers = []string{"revoke", "not a member", "not a channel member", "no longer a member", "access denied", "unauthorized", "forbidden", "archiv", "not found", "does not exist"}
var buzzHiddenContextMarkers = []string{"<memory>", "<durable_context_data>", "<system-reminder>"}

// BuzzConfig controls a Buzz relay connection and Nostr event policy.
type BuzzConfig struct {
	RelayURL            string
	PrivateKey          string
	RelayPublicKey      string
	AllowedUsers        []string
	RequireMention      bool
	MentionFreeChannels []string
	RequestTimeout      time.Duration
	MaxAttempts         int
	Client              *http.Client
	Sleep               func(context.Context, time.Duration) error
	Now                 func() time.Time
}

// Buzz is a signed, reconnecting Nostr relay provider.
type Buzz struct {
	relayURL       string
	workspaceID    string
	keys           buzzKeys
	relayPublicKey string
	allowedUsers   map[string]struct{}
	requireMention bool
	mentionFree    map[string]struct{}
	timeout        time.Duration
	attempts       int
	client         *http.Client
	sleep          func(context.Context, time.Duration) error
	now            func() time.Time
	dial           providerSocketDialer
	ownedClient    bool

	routes  *routeStore[buzzReplyRoute]
	engaged *routeStore[struct{}]

	startMu  sync.Mutex
	writeMu  sync.Mutex
	mu       sync.Mutex
	started  bool
	closed   bool
	cancel   context.CancelFunc
	done     chan struct{}
	socket   providerSocket
	once     sync.Once
	closeErr error

	connectionStarted time.Time
	pendingChallenge  string
	pendingAuthID     string
	authCompleted     bool
	metadata          map[string]buzzChannelMetadata
	metadataOrder     []string
	watermarks        map[string]int64
	watermarkOrder    []string
	subscriptions     map[string]struct{}
	resubscribe       map[string]int
}

type buzzReplyRoute struct {
	Author string
}

type buzzChannelMetadata struct {
	Name string
	Type string
}

// NewBuzz validates configuration and constructs a Buzz provider.
func NewBuzz(config BuzzConfig) (*Buzz, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.RelayURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		(parsed.Scheme != "wss" && parsed.Scheme != "ws") {
		return nil, ErrInvalid
	}
	keys, err := parseBuzzPrivateKey(config.PrivateKey)
	if err != nil {
		return nil, err
	}
	relayPublicKey := ""
	if strings.TrimSpace(config.RelayPublicKey) != "" {
		relayPublicKey, err = parseBuzzPublicKey(config.RelayPublicKey)
		if err != nil {
			return nil, err
		}
	}
	allowedUsers, err := parseBuzzPublicKeys(config.AllowedUsers)
	if err != nil || !validChannelValueSet(config.MentionFreeChannels) {
		return nil, ErrInvalid
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 20 * time.Second
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 3
	}
	if config.RequestTimeout < time.Second || config.RequestTimeout > 2*time.Minute || !validProviderAttempts(config.MaxAttempts) {
		return nil, ErrInvalid
	}
	injected := config.Client != nil
	if config.Client == nil {
		config.Client = newProviderClient(config.RequestTimeout)
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Buzz{
		relayURL: parsed.String(), workspaceID: parsed.Host, keys: keys, relayPublicKey: relayPublicKey,
		allowedUsers: allowedUsers, requireMention: config.RequireMention, mentionFree: normalizedSet(config.MentionFreeChannels),
		timeout: config.RequestTimeout, attempts: config.MaxAttempts, client: config.Client, sleep: config.Sleep, now: config.Now,
		dial: dialProviderSocket, ownedClient: !injected,
		routes: newRouteStore[buzzReplyRoute](4096, 2*time.Hour, config.Now), engaged: newRouteStore[struct{}](8192, 24*time.Hour, config.Now),
		metadata: make(map[string]buzzChannelMetadata), watermarks: make(map[string]int64),
		subscriptions: make(map[string]struct{}), resubscribe: make(map[string]int),
	}, nil
}

func parseBuzzPublicKeys(values []string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		parsed, err := parseBuzzPublicKey(value)
		if err != nil {
			return nil, err
		}
		if _, duplicate := result[parsed]; duplicate {
			return nil, ErrInvalid
		}
		result[parsed] = struct{}{}
	}
	return result, nil
}

// Name returns the provider registry name.
func (*Buzz) Name() string { return BuzzProvider }

// Start opens the relay and control subscriptions before starting ingress.
func (provider *Buzz) Start(parent context.Context, submit SubmitFunc) error {
	if provider == nil || parent == nil || submit == nil {
		return ErrInvalid
	}
	provider.startMu.Lock()
	defer provider.startMu.Unlock()
	provider.mu.Lock()
	if provider.closed || provider.started {
		closed := provider.closed
		provider.mu.Unlock()
		if closed {
			return ErrClosed
		}
		return nil
	}
	provider.mu.Unlock()
	socket, err := provider.openSocket(parent)
	if err != nil {
		return fmt.Errorf("open Buzz relay: %w", err)
	}
	provider.mu.Lock()
	if provider.closed || provider.started {
		closed := provider.closed
		provider.mu.Unlock()
		_ = socket.Close()
		if closed {
			return ErrClosed
		}
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	provider.cancel, provider.done, provider.socket, provider.started = cancel, make(chan struct{}), socket, true
	done := provider.done
	provider.mu.Unlock()
	go func() {
		defer close(done)
		provider.run(ctx, submit, socket)
	}()
	return nil
}

// Send publishes signed kind-9 reply chunks to the current relay connection.
func (provider *Buzz) Send(ctx context.Context, reply Reply) error {
	if provider == nil || ctx == nil || reply.Provider != BuzzProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	if buzzHiddenContext(reply.Text) != "" {
		provider.routes.Delete(reply.InReplyTo)
		return nil
	}
	route, _ := provider.routes.Get(reply.InReplyTo)
	chunks := splitBuzzText(reply.Text, buzzEditMaxBytes)
	for index, chunk := range chunks {
		replyTo := reply.TopicID
		requester := ""
		if index == 0 {
			requester = route.Author
		}
		marker := reply.InReplyTo + ":" + strconv.Itoa(index)
		event, err := buzzReplyEvent(provider.keys, reply.ChatID, chunk, provider.now(), replyTo, requester, marker)
		if err != nil {
			return err
		}
		if err = retryProvider(ctx, provider.attempts, provider.sleep, func() error { return provider.postEvent(ctx, event) }); err != nil {
			return fmt.Errorf("send Buzz message: %w", err)
		}
	}
	provider.routes.Delete(reply.InReplyTo)
	return nil
}

// Close stops reconnects and releases the current relay socket.
func (provider *Buzz) Close() error {
	if provider == nil {
		return nil
	}
	provider.once.Do(func() {
		provider.mu.Lock()
		provider.closed = true
		cancel, socket, done := provider.cancel, provider.socket, provider.done
		provider.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if socket != nil {
			provider.closeErr = socket.Close()
		}
		if done != nil {
			<-done
		}
		if provider.ownedClient {
			provider.client.CloseIdleConnections()
		}
	})
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.closeErr
}

func (provider *Buzz) openSocket(ctx context.Context) (providerSocket, error) {
	openCtx, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	socket, err := provider.dial(openCtx, provider.relayURL, provider.client)
	if err != nil {
		return nil, err
	}
	provider.resetConnectionState()
	if err = provider.openControlSubscriptions(openCtx, socket); err != nil {
		_ = socket.Close()
		return nil, err
	}
	return socket, nil
}

func (provider *Buzz) run(ctx context.Context, submit SubmitFunc, socket providerSocket) {
	for ctx.Err() == nil {
		reconnect := provider.serve(ctx, submit, socket)
		_ = socket.Close()
		provider.clearSocket(socket)
		if !reconnect || ctx.Err() != nil {
			return
		}
		for failures := 1; ctx.Err() == nil; failures++ {
			if provider.sleep(ctx, providerReconnectDelay(failures)) != nil {
				return
			}
			next, err := provider.openSocket(ctx)
			if err != nil {
				continue
			}
			socket = next
			provider.setSocket(socket)
			break
		}
	}
}

func (provider *Buzz) serve(ctx context.Context, submit SubmitFunc, socket providerSocket) bool {
	for ctx.Err() == nil {
		payload, err := socket.Read(ctx)
		if err != nil {
			return true
		}
		if err = provider.handleFrame(ctx, submit, payload); err != nil {
			if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) {
				return false
			}
			if errors.Is(err, ErrBusy) || isProviderNetworkError(err) {
				return true
			}
		}
	}
	return false
}

func isProviderNetworkError(err error) bool {
	var networkFailure *providerNetworkError
	return errors.As(err, &networkFailure)
}

func (provider *Buzz) handleFrame(ctx context.Context, submit SubmitFunc, payload []byte) error {
	var frame []json.RawMessage
	if !decodeBuzzJSON(payload, &frame) || len(frame) == 0 {
		return nil
	}
	var kind string
	if !decodeBuzzJSON(frame[0], &kind) {
		return nil
	}
	switch kind {
	case "AUTH":
		return provider.handleAuth(ctx, frame)
	case "OK":
		provider.handleAuthOK(frame)
	case "EOSE":
		return provider.handleEOSE(ctx, frame)
	case "CLOSED":
		return provider.handleClosed(ctx, frame)
	case "EVENT":
		return provider.handleEvent(ctx, submit, frame)
	}
	return nil
}

func (provider *Buzz) handleAuth(ctx context.Context, frame []json.RawMessage) error {
	if len(frame) < 2 {
		return nil
	}
	var challenge string
	if !decodeBuzzJSON(frame[1], &challenge) || challenge == "" || len(challenge) > 4096 {
		return nil
	}
	event, err := buzzAuthEvent(provider.keys, provider.relayURL, challenge, provider.now())
	if err != nil {
		return err
	}
	provider.mu.Lock()
	provider.pendingChallenge, provider.pendingAuthID, provider.authCompleted = challenge, event.ID, false
	provider.subscriptions, provider.resubscribe = make(map[string]struct{}), make(map[string]int)
	socket := provider.socket
	provider.mu.Unlock()
	if socket == nil {
		return &providerNetworkError{message: "Buzz socket is not connected"}
	}
	payload, err := buzzFrame("AUTH", event)
	if err == nil {
		err = provider.write(ctx, socket, payload)
	}
	if err != nil {
		return err
	}
	return provider.openControlSubscriptions(ctx, socket)
}

func (provider *Buzz) handleAuthOK(frame []json.RawMessage) {
	if len(frame) < 3 {
		return
	}
	var eventID string
	var accepted bool
	if json.Unmarshal(frame[1], &eventID) != nil || json.Unmarshal(frame[2], &accepted) != nil {
		return
	}
	provider.mu.Lock()
	if eventID == provider.pendingAuthID {
		provider.pendingAuthID = ""
		provider.authCompleted = accepted
	}
	provider.mu.Unlock()
}

func (provider *Buzz) handleEOSE(ctx context.Context, frame []json.RawMessage) error {
	if len(frame) < 2 {
		return nil
	}
	var subscriptionID string
	if !decodeBuzzJSON(frame[1], &subscriptionID) || subscriptionID != buzzDiscoverySubscription {
		return nil
	}
	provider.mu.Lock()
	if provider.pendingAuthID != "" {
		provider.pendingAuthID, provider.authCompleted = "", true
	}
	channels := append([]string(nil), provider.metadataOrder...)
	provider.mu.Unlock()
	for _, channelID := range channels {
		if err := provider.ensureChatSubscription(ctx, channelID); err != nil {
			return err
		}
	}
	return nil
}

func (provider *Buzz) handleClosed(ctx context.Context, frame []json.RawMessage) error {
	if len(frame) < 2 {
		return nil
	}
	var subscriptionID, reason string
	if !decodeBuzzJSON(frame[1], &subscriptionID) {
		return nil
	}
	if len(frame) >= 3 {
		_ = json.Unmarshal(frame[2], &reason)
	}
	provider.mu.Lock()
	authCompleted := provider.authCompleted
	provider.mu.Unlock()
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(reason)), "auth-required:") && !authCompleted {
		if strings.HasPrefix(subscriptionID, buzzChatSubscriptionPrefix) {
			provider.removeSubscription(strings.TrimPrefix(subscriptionID, buzzChatSubscriptionPrefix))
		}
		return nil
	}
	if subscriptionID == buzzDiscoverySubscription || subscriptionID == buzzMembershipSubscription {
		return provider.recoverControlSubscription(ctx, subscriptionID, reason)
	}
	if strings.HasPrefix(subscriptionID, buzzChatSubscriptionPrefix) {
		return provider.recoverChatSubscription(ctx, strings.TrimPrefix(subscriptionID, buzzChatSubscriptionPrefix), reason)
	}
	return nil
}

func (provider *Buzz) handleEvent(ctx context.Context, submit SubmitFunc, frame []json.RawMessage) error {
	if len(frame) < 3 {
		return nil
	}
	var event buzzEvent
	if !decodeBuzzJSON(frame[2], &event) || !verifyBuzzEvent(event) {
		return nil
	}
	if provider.relayPublicKey != "" && event.Kind != buzzKindChat && event.PubKey != provider.relayPublicKey {
		return nil
	}
	switch event.Kind {
	case buzzKindChannelMeta:
		if channelID := provider.cacheChannelMetadata(event); channelID != "" {
			return provider.ensureChatSubscription(ctx, channelID)
		}
	case buzzKindMemberAdded, buzzKindMemberRemoved:
		return provider.handleMembershipEvent(ctx, event)
	case buzzKindChat:
		return provider.handleChatEvent(ctx, submit, event)
	}
	return nil
}

func (provider *Buzz) handleChatEvent(ctx context.Context, submit SubmitFunc, event buzzEvent) error {
	channels := buzzTagValues(event, "h")
	if len(channels) == 0 || event.PubKey == provider.keys.Public || !listed(provider.allowedUsers, event.PubKey) {
		return nil
	}
	channelID := channels[0]
	threadRoot := firstBuzzTag(event, "e")
	if len(channelID) > 256 || len(threadRoot) > 256 {
		return nil
	}
	mentioned := containsExact(buzzTagValues(event, "p"), provider.keys.Public)
	engagementKey := buzzEngagementKey(event.PubKey, channelID, threadRoot)
	_, engaged := provider.engaged.Get(engagementKey)
	provider.mu.Lock()
	metadata := provider.metadata[channelID]
	provider.mu.Unlock()
	if provider.requireMention && !mentioned && !listed(provider.mentionFree, channelID) && metadata.Type != "dm" && !engaged {
		return nil
	}
	text := strings.TrimSpace(event.Content)
	if mentioned {
		text = stripBuzzMention(text)
	}
	if text == "" {
		return nil
	}
	message := Message{
		ID: event.ID, Provider: BuzzProvider, WorkspaceID: provider.workspaceID,
		ExternalUserID: event.PubKey, ChatID: channelID, TopicID: threadRoot, Text: text,
		Metadata:   map[string]string{"event_id": event.ID, "channel_name": metadata.Name},
		ReceivedAt: time.Unix(event.CreatedAt, 0).UTC(),
	}
	provider.routes.Put(event.ID, buzzReplyRoute{Author: event.PubKey})
	if err := submitProviderMessage(ctx, submit, message); err != nil && !errors.Is(err, ErrDuplicate) {
		provider.routes.Delete(event.ID)
		return err
	}
	provider.engaged.Put(engagementKey, struct{}{})
	provider.advanceWatermark(channelID, event.CreatedAt)
	return nil
}

func (provider *Buzz) cacheChannelMetadata(event buzzEvent) string {
	channelID := firstBuzzTag(event, "d")
	if channelID == "" || len(channelID) > 256 {
		return ""
	}
	metadata := buzzChannelMetadata{Name: truncateBuzzValue(firstBuzzTag(event, "name"), 512), Type: truncateBuzzValue(firstBuzzTag(event, "t"), 64)}
	if metadata.Type == "" {
		metadata.Type = "stream"
	}
	provider.mu.Lock()
	if _, exists := provider.metadata[channelID]; !exists {
		provider.metadataOrder = append(provider.metadataOrder, channelID)
	}
	provider.metadata[channelID] = metadata
	for len(provider.metadataOrder) > buzzMaxCachedChannels {
		oldest := provider.metadataOrder[0]
		provider.metadataOrder = provider.metadataOrder[1:]
		delete(provider.metadata, oldest)
	}
	provider.mu.Unlock()
	return channelID
}

func (provider *Buzz) handleMembershipEvent(ctx context.Context, event buzzEvent) error {
	if !containsExact(buzzTagValues(event, "p"), provider.keys.Public) {
		return nil
	}
	channelID := firstBuzzTag(event, "h")
	if channelID == "" {
		return nil
	}
	if event.Kind == buzzKindMemberRemoved {
		provider.mu.Lock()
		delete(provider.metadata, channelID)
		provider.metadataOrder = removeBuzzString(provider.metadataOrder, channelID)
		provider.mu.Unlock()
		return provider.closeChatSubscription(ctx, channelID)
	}
	if err := provider.ensureChatSubscription(ctx, channelID); err != nil {
		return err
	}
	provider.mu.Lock()
	_, known := provider.metadata[channelID]
	socket := provider.socket
	provider.mu.Unlock()
	if known || socket == nil {
		return nil
	}
	payload, err := buzzFrame("REQ", buzzDiscoverySubscription, map[string]any{"kinds": []int{buzzKindChannelMeta}})
	if err != nil {
		return err
	}
	return provider.write(ctx, socket, payload)
}

func (provider *Buzz) openControlSubscriptions(ctx context.Context, socket providerSocket) error {
	provider.mu.Lock()
	started := provider.connectionStarted
	provider.mu.Unlock()
	since := started.Add(-buzzMembershipLookback).Unix()
	discovery, err := buzzFrame("REQ", buzzDiscoverySubscription, map[string]any{"kinds": []int{buzzKindChannelMeta}})
	if err != nil {
		return err
	}
	membership, err := buzzFrame("REQ", buzzMembershipSubscription, map[string]any{
		"kinds": []int{buzzKindMemberAdded, buzzKindMemberRemoved}, "#p": []string{provider.keys.Public}, "since": max(int64(0), since),
	})
	if err != nil {
		return err
	}
	if err = provider.write(ctx, socket, discovery); err != nil {
		return err
	}
	return provider.write(ctx, socket, membership)
}

func (provider *Buzz) ensureChatSubscription(ctx context.Context, channelID string) error {
	if channelID == "" || len(channelID) > 256 {
		return nil
	}
	provider.mu.Lock()
	if _, exists := provider.subscriptions[channelID]; exists || len(provider.subscriptions) >= buzzMaxChannelSubscriptions || provider.socket == nil {
		provider.mu.Unlock()
		return nil
	}
	socket := provider.socket
	watermark := provider.watermarks[channelID]
	provider.mu.Unlock()
	filter := map[string]any{"kinds": []int{buzzKindChat}, "#h": []string{channelID}}
	if watermark > 0 {
		filter["since"] = watermark
	}
	payload, err := buzzFrame("REQ", buzzChatSubscriptionPrefix+channelID, filter)
	if err != nil {
		return err
	}
	if err = provider.write(ctx, socket, payload); err != nil {
		return err
	}
	provider.mu.Lock()
	if provider.socket == socket {
		provider.subscriptions[channelID] = struct{}{}
	}
	provider.mu.Unlock()
	return nil
}

func (provider *Buzz) closeChatSubscription(ctx context.Context, channelID string) error {
	provider.mu.Lock()
	_, exists := provider.subscriptions[channelID]
	delete(provider.subscriptions, channelID)
	socket := provider.socket
	provider.mu.Unlock()
	if !exists || socket == nil {
		return nil
	}
	payload, err := buzzFrame("CLOSE", buzzChatSubscriptionPrefix+channelID)
	if err != nil {
		return err
	}
	return provider.write(ctx, socket, payload)
}

func (provider *Buzz) recoverControlSubscription(ctx context.Context, subscriptionID, reason string) error {
	if !buzzTransientClose(reason) {
		return nil
	}
	attempt := provider.claimResubscribe(subscriptionID)
	if attempt == 0 {
		return nil
	}
	if attempt > 1 {
		if err := provider.sleep(ctx, time.Duration(1<<(attempt-2))*time.Second); err != nil {
			return err
		}
	}
	provider.mu.Lock()
	socket := provider.socket
	provider.mu.Unlock()
	if socket == nil {
		return nil
	}
	if subscriptionID == buzzDiscoverySubscription {
		payload, err := buzzFrame("REQ", subscriptionID, map[string]any{"kinds": []int{buzzKindChannelMeta}})
		if err != nil {
			return err
		}
		return provider.write(ctx, socket, payload)
	}
	provider.mu.Lock()
	started := provider.connectionStarted
	provider.mu.Unlock()
	payload, err := buzzFrame("REQ", subscriptionID, map[string]any{
		"kinds": []int{buzzKindMemberAdded, buzzKindMemberRemoved}, "#p": []string{provider.keys.Public},
		"since": max(int64(0), started.Add(-buzzMembershipLookback).Unix()),
	})
	if err != nil {
		return err
	}
	return provider.write(ctx, socket, payload)
}

func (provider *Buzz) recoverChatSubscription(ctx context.Context, channelID, reason string) error {
	provider.mu.Lock()
	_, subscribed := provider.subscriptions[channelID]
	delete(provider.subscriptions, channelID)
	provider.mu.Unlock()
	if !subscribed || !buzzTransientClose(reason) {
		return nil
	}
	attempt := provider.claimResubscribe(buzzChatSubscriptionPrefix + channelID)
	if attempt == 0 {
		return nil
	}
	if attempt > 1 {
		if err := provider.sleep(ctx, time.Duration(1<<(attempt-2))*time.Second); err != nil {
			return err
		}
	}
	return provider.ensureChatSubscription(ctx, channelID)
}

func (provider *Buzz) claimResubscribe(subscriptionID string) int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	used := provider.resubscribe[subscriptionID]
	if used >= buzzMaxResubscribeAttempts {
		return 0
	}
	provider.resubscribe[subscriptionID] = used + 1
	if len(provider.resubscribe) > buzzMaxCachedChannels {
		for key := range provider.resubscribe {
			delete(provider.resubscribe, key)
			break
		}
	}
	return used + 1
}

func (provider *Buzz) advanceWatermark(channelID string, createdAt int64) {
	if channelID == "" || createdAt <= 0 || createdAt > provider.now().Add(buzzMaxFutureSkew).Unix() {
		return
	}
	provider.mu.Lock()
	if _, exists := provider.watermarks[channelID]; !exists {
		provider.watermarkOrder = append(provider.watermarkOrder, channelID)
	}
	provider.watermarks[channelID] = max(provider.watermarks[channelID], createdAt)
	for len(provider.watermarkOrder) > buzzMaxCachedChannels {
		oldest := provider.watermarkOrder[0]
		provider.watermarkOrder = provider.watermarkOrder[1:]
		delete(provider.watermarks, oldest)
	}
	provider.mu.Unlock()
}

func (provider *Buzz) resetConnectionState() {
	provider.mu.Lock()
	provider.connectionStarted = provider.now().UTC()
	provider.pendingChallenge, provider.pendingAuthID, provider.authCompleted = "", "", false
	provider.subscriptions, provider.resubscribe = make(map[string]struct{}), make(map[string]int)
	provider.mu.Unlock()
}

func (provider *Buzz) removeSubscription(channelID string) {
	provider.mu.Lock()
	delete(provider.subscriptions, channelID)
	provider.mu.Unlock()
}

func (provider *Buzz) postEvent(ctx context.Context, event buzzEvent) error {
	provider.mu.Lock()
	socket := provider.socket
	provider.mu.Unlock()
	if socket == nil {
		return &providerNetworkError{message: "Buzz socket is not connected"}
	}
	payload, err := buzzFrame("EVENT", event)
	if err != nil {
		return err
	}
	return provider.write(ctx, socket, payload)
}

func (provider *Buzz) write(ctx context.Context, socket providerSocket, payload []byte) error {
	provider.writeMu.Lock()
	defer provider.writeMu.Unlock()
	if err := socket.Write(ctx, payload); err != nil {
		return sanitizeProviderNetworkError(err)
	}
	return nil
}

func (provider *Buzz) setSocket(socket providerSocket) {
	provider.mu.Lock()
	if !provider.closed {
		provider.socket = socket
	}
	provider.mu.Unlock()
}

func (provider *Buzz) clearSocket(socket providerSocket) {
	provider.mu.Lock()
	if provider.socket == socket {
		provider.socket = nil
	}
	provider.subscriptions = make(map[string]struct{})
	provider.resubscribe = make(map[string]int)
	provider.authCompleted, provider.pendingAuthID, provider.pendingChallenge = false, "", ""
	provider.mu.Unlock()
}

func firstBuzzTag(event buzzEvent, name string) string {
	values := buzzTagValues(event, name)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func buzzEngagementKey(author, channelID, topicID string) string {
	return author + "\xff" + channelID + "\xff" + topicID
}

func stripBuzzMention(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "@") {
		return trimmed
	}
	_, rest, found := strings.Cut(trimmed, " ")
	if !found || strings.HasPrefix(strings.TrimSpace(rest), "@") || strings.TrimSpace(rest) == "" {
		return trimmed
	}
	return strings.TrimSpace(rest)
}

func splitBuzzText(text string, limit int) []string {
	if limit < 1 {
		return nil
	}
	chunks, current := make([]string, 0), strings.Builder{}
	size := 0
	for _, character := range text {
		width := utf8.RuneLen(character)
		if size+width > limit && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
			size = 0
		}
		current.WriteRune(character)
		size += width
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func buzzHiddenContext(text string) string {
	lowered := strings.ToLower(text)
	for _, marker := range buzzHiddenContextMarkers {
		if strings.Contains(lowered, marker) {
			return marker
		}
	}
	return ""
}

func buzzTransientClose(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	for _, prefix := range buzzPermanentClosePrefixes {
		if strings.HasPrefix(reason, prefix) {
			return false
		}
	}
	for _, marker := range buzzPermanentCloseMarkers {
		if strings.Contains(reason, marker) {
			return false
		}
	}
	return true
}

func truncateBuzzValue(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func removeBuzzString(values []string, target string) []string {
	for index, value := range values {
		if value == target {
			return append(values[:index], values[index+1:]...)
		}
	}
	return values
}

func decodeBuzzJSON(data []byte, output any) bool {
	return json.Unmarshal(data, output) == nil
}
