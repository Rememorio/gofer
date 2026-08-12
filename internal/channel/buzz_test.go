package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuzzLifecycleDiscoveryIngressAndReply(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	author := mustBuzzKeys(t, buzzAuthorSecret)
	provider := newBuzzTestProvider(t, now, []string{author.Public})
	socket := newFakeProviderSocket(8)
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) { return socket, nil }
	submitted := make(chan Message, 1)
	if err := provider.Start(context.Background(), func(_ context.Context, message Message) error { submitted <- message; return nil }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	assertBuzzControlSubscriptions(t, waitBuzzWrites(t, socket, 2))
	metadata, _ := newBuzzEvent(author, buzzKindChannelMeta, [][]string{{"d", "channel-1"}, {"t", "dm"}, {"name", "Private"}}, "", now)
	socket.push([]any{"EVENT", buzzDiscoverySubscription, metadata})
	writes := waitBuzzWrites(t, socket, 3)
	assertBuzzREQ(t, writes[2], buzzChatSubscriptionPrefix+"channel-1", "#h", "channel-1")
	chat, _ := buzzChatEvent(author, "channel-1", "hello", now, "")
	socket.push([]any{"EVENT", buzzChatSubscriptionPrefix + "channel-1", chat})
	var message Message
	select {
	case message = <-submitted:
	case <-time.After(time.Second):
		t.Fatal("Buzz message was not submitted")
	}
	if message.ID != chat.ID || message.Provider != BuzzProvider || message.ExternalUserID != author.Public ||
		message.WorkspaceID != "relay.test" || message.ChatID != "channel-1" || message.Text != "hello" || message.Metadata["channel_name"] != "Private" {
		t.Fatalf("message = %#v", message)
	}
	if err := provider.Send(context.Background(), Reply{Provider: BuzzProvider, ChatID: "channel-1", InReplyTo: chat.ID, Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	reply := decodeBuzzEventFrame(t, waitBuzzWrites(t, socket, 4)[3])
	if !verifyBuzzEvent(reply) || reply.Kind != buzzKindChat || reply.Content != "answer" || firstBuzzTag(reply, "p") != author.Public {
		t.Fatalf("reply = %#v", reply)
	}
	provider.mu.Lock()
	watermark := provider.watermarks["channel-1"]
	provider.mu.Unlock()
	if watermark != now.Unix() {
		t.Fatalf("watermark = %d", watermark)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v", err)
	}
}

func TestBuzzNIP42AuthAndTrustedRelayEvents(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	relay := mustBuzzKeys(t, buzzAuthorSecret)
	provider := newBuzzTestProvider(t, now, nil)
	provider.relayPublicKey = relay.Public
	socket := newFakeProviderSocket(8)
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) { return socket, nil }
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	waitBuzzWrites(t, socket, 2)
	socket.push([]any{"AUTH", "challenge-1"})
	writes := waitBuzzWrites(t, socket, 5)
	var authFrame []json.RawMessage
	if json.Unmarshal(writes[2], &authFrame) != nil || len(authFrame) != 2 {
		t.Fatalf("auth frame = %s", writes[2])
	}
	var auth buzzEvent
	_ = json.Unmarshal(authFrame[1], &auth)
	if !verifyBuzzEvent(auth) || auth.Kind != buzzKindAuth || firstBuzzTag(auth, "relay") != "ws://relay.test" || firstBuzzTag(auth, "challenge") != "challenge-1" {
		t.Fatalf("auth event = %#v", auth)
	}
	socket.push([]any{"OK", auth.ID, true, "accepted"})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		provider.mu.Lock()
		authenticated := provider.authCompleted
		provider.mu.Unlock()
		if authenticated {
			break
		}
		time.Sleep(time.Millisecond)
	}
	provider.mu.Lock()
	authenticated := provider.authCompleted
	provider.mu.Unlock()
	if !authenticated {
		t.Fatal("NIP-42 acknowledgment was not recorded")
	}

	attacker := mustBuzzKeys(t, buzzTestSecret)
	forged, _ := newBuzzEvent(attacker, buzzKindChannelMeta, [][]string{{"d", "forged"}, {"t", "dm"}}, "", now)
	socket.push([]any{"EVENT", buzzDiscoverySubscription, forged})
	time.Sleep(20 * time.Millisecond)
	provider.mu.Lock()
	_, accepted := provider.metadata["forged"]
	provider.mu.Unlock()
	if accepted {
		t.Fatal("metadata from an untrusted relay key was accepted")
	}
	valid, _ := newBuzzEvent(relay, buzzKindChannelMeta, [][]string{{"d", "trusted"}, {"t", "stream"}}, "", now)
	socket.push([]any{"EVENT", buzzDiscoverySubscription, valid})
	waitBuzzWrites(t, socket, 6)
}

func TestBuzzMentionAllowlistEngagementAndWatermarkPolicy(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	author, blocked := mustBuzzKeys(t, buzzAuthorSecret), mustBuzzKeys(t, buzzBlockedSecret)
	provider := newBuzzTestProvider(t, now, []string{author.Public})
	provider.mu.Lock()
	provider.metadata["channel"] = buzzChannelMetadata{Type: "stream"}
	provider.mu.Unlock()
	submitted := make(chan Message, 4)
	submit := func(_ context.Context, message Message) error { submitted <- message; return nil }
	plain, _ := buzzChatEvent(author, "channel", "plain", now, "")
	if err := provider.handleChatEvent(context.Background(), submit, plain); err != nil {
		t.Fatal(err)
	}
	assertNoBuzzMessage(t, submitted)
	mentioned, _ := buzzChatEvent(author, "channel", "@Gofer please help", now, "root", provider.keys.Public)
	if err := provider.handleChatEvent(context.Background(), submit, mentioned); err != nil {
		t.Fatal(err)
	}
	if message := waitBuzzMessage(t, submitted); message.Text != "please help" || message.TopicID != "root" {
		t.Fatalf("mentioned message = %#v", message)
	}
	followup, _ := buzzChatEvent(author, "channel", "follow up", now.Add(time.Second), "root")
	if err := provider.handleChatEvent(context.Background(), submit, followup); err != nil {
		t.Fatal(err)
	}
	if message := waitBuzzMessage(t, submitted); message.Text != "follow up" {
		t.Fatalf("engaged follow-up = %#v", message)
	}
	unauthorized, _ := buzzChatEvent(blocked, "channel", "@Gofer attack", now, "", provider.keys.Public)
	_ = provider.handleChatEvent(context.Background(), submit, unauthorized)
	assertNoBuzzMessage(t, submitted)
	connecting, _ := buzzChatEvent(blocked, "channel", "/connect code", now, "")
	if err := provider.handleChatEvent(context.Background(), submit, connecting); err != nil {
		t.Fatal(err)
	}
	if message := waitBuzzMessage(t, submitted); message.Text != "/connect code" {
		t.Fatalf("disallowed connect = %#v", message)
	}
	provider.advanceWatermark("channel", now.Add(2*buzzMaxFutureSkew).Unix())
	provider.mu.Lock()
	watermark := provider.watermarks["channel"]
	provider.mu.Unlock()
	if watermark != now.Add(time.Second).Unix() {
		t.Fatalf("future event moved watermark to %d", watermark)
	}
}

func TestBuzzMembershipAndClosedSubscriptionRecovery(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	relay := mustBuzzKeys(t, buzzAuthorSecret)
	provider := newBuzzTestProvider(t, now, nil)
	socket := newFakeProviderSocket(8)
	provider.mu.Lock()
	provider.socket = socket
	provider.connectionStarted = now
	provider.mu.Unlock()
	added, _ := newBuzzEvent(relay, buzzKindMemberAdded, [][]string{{"p", provider.keys.Public}, {"h", "new-channel"}}, "", now)
	if err := provider.handleMembershipEvent(context.Background(), added); err != nil {
		t.Fatal(err)
	}
	writes := waitBuzzWrites(t, socket, 2)
	assertBuzzREQ(t, writes[0], buzzChatSubscriptionPrefix+"new-channel", "#h", "new-channel")
	provider.cacheChannelMetadata(buzzEvent{Tags: [][]string{{"d", "new-channel"}, {"t", "stream"}}})
	removed, _ := newBuzzEvent(relay, buzzKindMemberRemoved, [][]string{{"p", provider.keys.Public}, {"h", "new-channel"}}, "", now)
	if err := provider.handleMembershipEvent(context.Background(), removed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(waitBuzzWrites(t, socket, 3)[2]), `"CLOSE"`) {
		t.Fatal("membership removal did not close the chat subscription")
	}
	provider.mu.Lock()
	provider.subscriptions["transient"] = struct{}{}
	provider.mu.Unlock()
	if err := provider.recoverChatSubscription(context.Background(), "transient", "rate-limited: later"); err != nil {
		t.Fatal(err)
	}
	assertBuzzREQ(t, waitBuzzWrites(t, socket, 4)[3], buzzChatSubscriptionPrefix+"transient", "#h", "transient")
	provider.mu.Lock()
	provider.subscriptions["removed"] = struct{}{}
	provider.mu.Unlock()
	before := len(socket.written())
	if err := provider.recoverChatSubscription(context.Background(), "removed", "error: not a member"); err != nil {
		t.Fatal(err)
	}
	if len(socket.written()) != before {
		t.Fatal("permanently refused subscription was reopened")
	}
}

func TestBuzzRelayFrameControlRecoveryAndMalformedInput(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	provider := newBuzzTestProvider(t, now, nil)
	socket := newFakeProviderSocket(1)
	provider.mu.Lock()
	provider.socket = socket
	provider.connectionStarted = now
	provider.pendingAuthID = "auth-event"
	provider.metadata["known"] = buzzChannelMetadata{Type: "stream"}
	provider.metadataOrder = []string{"known"}
	provider.mu.Unlock()
	ctx := context.Background()
	submit := func(context.Context, Message) error { return nil }
	if err := provider.handleFrame(ctx, submit, []byte(`not-json`)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{[]byte(`[]`), []byte(`[1]`), []byte(`{"x":true}`), []byte(`["AUTH",7]`), []byte(`["EOSE"]`), []byte(`["CLOSED"]`), []byte(`["EVENT","sub",{}]`)} {
		if err := provider.handleFrame(ctx, submit, payload); err != nil {
			t.Fatalf("malformed frame %s = %v", payload, err)
		}
	}
	if err := provider.handleFrame(ctx, submit, []byte(`["EOSE","buzz-discovery"]`)); err != nil {
		t.Fatal(err)
	}
	assertBuzzREQ(t, waitBuzzWrites(t, socket, 1)[0], buzzChatSubscriptionPrefix+"known", "#h", "known")
	provider.mu.Lock()
	if !provider.authCompleted || provider.pendingAuthID != "" {
		provider.mu.Unlock()
		t.Fatal("discovery EOSE did not confirm pending authentication")
	}
	provider.mu.Unlock()
	if err := provider.handleFrame(ctx, submit, []byte(`["CLOSED","buzz-discovery","rate-limited: retry"]`)); err != nil {
		t.Fatal(err)
	}
	assertBuzzREQ(t, waitBuzzWrites(t, socket, 2)[1], buzzDiscoverySubscription, "kinds", "39000")
	provider.mu.Lock()
	provider.authCompleted = false
	provider.subscriptions["bootstrap"] = struct{}{}
	provider.mu.Unlock()
	before := len(socket.written())
	if err := provider.handleFrame(ctx, submit, []byte(`["CLOSED","buzz-chat-bootstrap","auth-required: login"]`)); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	_, subscribed := provider.subscriptions["bootstrap"]
	provider.mu.Unlock()
	if subscribed || len(socket.written()) != before {
		t.Fatal("pre-auth CLOSED was not handled as bootstrap state")
	}
	provider.mu.Lock()
	provider.authCompleted = true
	provider.mu.Unlock()
	if err := provider.handleFrame(ctx, submit, []byte(`["CLOSED","buzz-membership","restricted: denied"]`)); err != nil {
		t.Fatal(err)
	}
	if len(socket.written()) != before {
		t.Fatal("permanent control refusal was retried")
	}
}

func TestBuzzServeStopsOnClosedIngressAndResubscribeBudgetIsBounded(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	author := mustBuzzKeys(t, buzzAuthorSecret)
	provider := newBuzzTestProvider(t, now, []string{author.Public})
	socket := newFakeProviderSocket(2)
	provider.mu.Lock()
	provider.socket = socket
	provider.metadata["dm"] = buzzChannelMetadata{Type: "dm"}
	provider.mu.Unlock()
	event, _ := buzzChatEvent(author, "dm", "hello", now, "")
	socket.push([]any{"EVENT", buzzChatSubscriptionPrefix + "dm", event})
	if reconnect := provider.serve(context.Background(), func(context.Context, Message) error { return ErrClosed }, socket); reconnect {
		t.Fatal("closed manager requested a relay reconnect")
	}
	for attempt := 1; attempt <= buzzMaxResubscribeAttempts; attempt++ {
		if got := provider.claimResubscribe("subscription"); got != attempt {
			t.Fatalf("attempt %d = %d", attempt, got)
		}
	}
	if got := provider.claimResubscribe("subscription"); got != 0 {
		t.Fatalf("exhausted retry budget = %d", got)
	}
	for index := 0; index <= buzzMaxCachedChannels; index++ {
		provider.claimResubscribe("sub-" + strconv.Itoa(index))
	}
	provider.mu.Lock()
	count := len(provider.resubscribe)
	provider.mu.Unlock()
	if count > buzzMaxCachedChannels {
		t.Fatalf("resubscribe cache grew to %d", count)
	}
}

func TestBuzzSendSplittingSafetyAndFailures(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	provider := newBuzzTestProvider(t, now, nil)
	socket := newFakeProviderSocket(1)
	provider.mu.Lock()
	provider.socket = socket
	provider.mu.Unlock()
	provider.routes.Put("message", buzzReplyRoute{Author: mustBuzzKeys(t, buzzAuthorSecret).Public})
	text := strings.Repeat("界", buzzEditMaxBytes/3) + "a"
	if err := provider.Send(context.Background(), Reply{Provider: BuzzProvider, ChatID: "channel", TopicID: "root", InReplyTo: "message", Text: text}); err != nil {
		t.Fatal(err)
	}
	writes := waitBuzzWrites(t, socket, 2)
	first, second := decodeBuzzEventFrame(t, writes[0]), decodeBuzzEventFrame(t, writes[1])
	if len([]byte(first.Content)) > buzzEditMaxBytes || len([]byte(second.Content)) > buzzEditMaxBytes ||
		first.ID == second.ID || firstBuzzTag(first, "p") == "" || firstBuzzTag(second, "p") != "" || firstBuzzTag(second, "e") != "root" ||
		firstBuzzTag(first, "client") != "gofer" {
		t.Fatalf("split events = %#v / %#v", first.Tags, second.Tags)
	}
	before := len(socket.written())
	if err := provider.Send(context.Background(), Reply{Provider: BuzzProvider, ChatID: "channel", Text: "<memory>secret</memory>"}); err != nil || len(socket.written()) != before {
		t.Fatalf("hidden context send = %v, writes=%d", err, len(socket.written()))
	}
	provider.mu.Lock()
	provider.socket = nil
	provider.mu.Unlock()
	if err := provider.Send(context.Background(), Reply{Provider: BuzzProvider, ChatID: "channel", Text: "answer"}); err == nil {
		t.Fatal("send without relay connection succeeded")
	}
	if err := provider.Send(context.Background(), Reply{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid Send = %v", err)
	}
}

func TestBuzzValidationReconnectAndHelpers(t *testing.T) {
	t.Parallel()
	valid := BuzzConfig{RelayURL: "ws://relay.test", PrivateKey: buzzTestSecret, Client: &http.Client{}, RequestTimeout: time.Second, MaxAttempts: 1}
	for _, config := range []BuzzConfig{
		{}, {RelayURL: "https://relay.test", PrivateKey: buzzTestSecret},
		{RelayURL: "ws://user@relay.test", PrivateKey: buzzTestSecret},
		{RelayURL: "ws://relay.test", PrivateKey: "bad"},
		{RelayURL: "ws://relay.test", PrivateKey: buzzTestSecret, RelayPublicKey: "bad"},
		{RelayURL: "ws://relay.test", PrivateKey: buzzTestSecret, RequestTimeout: time.Millisecond},
		{RelayURL: "ws://relay.test", PrivateKey: buzzTestSecret, MaxAttempts: 6},
	} {
		if _, err := NewBuzz(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewBuzz(%#v) = %v", config, err)
		}
	}
	provider, err := NewBuzz(valid)
	if err != nil || provider.Name() != BuzzProvider {
		t.Fatalf("NewBuzz(valid) = %#v, %v", provider, err)
	}
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) { return nil, errors.New("offline") }
	if err = provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("dial failure = %v", err)
	}
	first, second := newFakeProviderSocket(1), newFakeProviderSocket(1)
	var mutex sync.Mutex
	dials := 0
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) {
		mutex.Lock()
		defer mutex.Unlock()
		dials++
		if dials == 1 {
			return first, nil
		}
		return second, nil
	}
	provider.sleep = func(context.Context, time.Duration) error { return nil }
	if err = provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	waitBuzzWrites(t, first, 2)
	_ = first.Close()
	waitBuzzWrites(t, second, 2)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		provider.mu.Lock()
		current := provider.socket
		provider.mu.Unlock()
		if current == second {
			break
		}
		time.Sleep(time.Millisecond)
	}
	provider.mu.Lock()
	current := provider.socket
	provider.mu.Unlock()
	if current != second {
		t.Fatal("reconnected Buzz socket was not installed")
	}
	if err = provider.Close(); err != nil || (*Buzz)(nil).Close() != nil {
		t.Fatalf("Close = %v", err)
	}
	if !buzzTransientClose("rate-limited: retry") || buzzTransientClose("error: channel not found") ||
		stripBuzzMention("@Bot hello") != "hello" || stripBuzzMention("@A @Bot hello") != "@A @Bot hello" ||
		buzzHiddenContext("<SYSTEM-REMINDER>x") != "<system-reminder>" {
		t.Fatal("Buzz helper policy failed")
	}
}

func newBuzzTestProvider(t *testing.T, now time.Time, allowedUsers []string) *Buzz {
	t.Helper()
	provider, err := NewBuzz(BuzzConfig{
		RelayURL: "ws://relay.test", PrivateKey: buzzTestSecret, AllowedUsers: allowedUsers,
		RequireMention: true, RequestTimeout: time.Second, MaxAttempts: 1,
		Client: &http.Client{}, Sleep: func(context.Context, time.Duration) error { return nil }, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func mustBuzzKeys(t *testing.T, secret string) buzzKeys {
	t.Helper()
	keys, err := parseBuzzPrivateKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func waitBuzzWrites(t *testing.T, socket *fakeProviderSocket, count int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if writes := socket.written(); len(writes) >= count {
			return writes
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d Buzz writes", count)
	return nil
}

func assertBuzzControlSubscriptions(t *testing.T, writes [][]byte) {
	t.Helper()
	assertBuzzREQ(t, writes[0], buzzDiscoverySubscription, "kinds", "39000")
	assertBuzzREQ(t, writes[1], buzzMembershipSubscription, "#p", "")
}

func assertBuzzREQ(t *testing.T, payload []byte, subscriptionID, field, value string) {
	t.Helper()
	var frame []json.RawMessage
	if json.Unmarshal(payload, &frame) != nil || len(frame) != 3 || string(frame[0]) != `"REQ"` || string(frame[1]) != `"`+subscriptionID+`"` ||
		!strings.Contains(string(frame[2]), `"`+field+`"`) || value != "" && !strings.Contains(string(frame[2]), value) {
		t.Fatalf("REQ = %s", payload)
	}
}

func decodeBuzzEventFrame(t *testing.T, payload []byte) buzzEvent {
	t.Helper()
	var frame []json.RawMessage
	if json.Unmarshal(payload, &frame) != nil || len(frame) != 2 || string(frame[0]) != `"EVENT"` {
		t.Fatalf("event frame = %s", payload)
	}
	var event buzzEvent
	if json.Unmarshal(frame[1], &event) != nil {
		t.Fatalf("event payload = %s", frame[1])
	}
	return event
}

func waitBuzzMessage(t *testing.T, messages <-chan Message) Message {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Buzz message")
		return Message{}
	}
}

func assertNoBuzzMessage(t *testing.T, messages <-chan Message) {
	t.Helper()
	select {
	case message := <-messages:
		t.Fatalf("unexpected Buzz message = %#v", message)
	case <-time.After(20 * time.Millisecond):
	}
}
