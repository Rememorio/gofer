package channel

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/bech32"
)

const buzzTestSecret = "0000000000000000000000000000000000000000000000000000000000000001"
const buzzAuthorSecret = "0000000000000000000000000000000000000000000000000000000000000002"
const buzzBlockedSecret = "0000000000000000000000000000000000000000000000000000000000000003"

func TestBuzzNostrKeyFormats(t *testing.T) {
	t.Parallel()
	keys, err := parseBuzzPrivateKey(buzzTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	nsec := encodeBuzzTestKey(t, "nsec", buzzTestSecret)
	bech32Keys, err := parseBuzzPrivateKey(nsec)
	if err != nil || bech32Keys != keys {
		t.Fatalf("nsec parse = %#v, %v", bech32Keys, err)
	}
	npub := encodeBuzzTestKey(t, "npub", keys.Public)
	if public, parseErr := parseBuzzPublicKey(npub); parseErr != nil || public != keys.Public {
		t.Fatalf("npub parse = %q, %v", public, parseErr)
	}
}

func TestBuzzNostrEventsFramesAndCanonicalJSON(t *testing.T) {
	t.Parallel()
	keys, err := parseBuzzPrivateKey(buzzTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_800_000_000, 0)
	vector, err := buzzChatEvent(keys, "channel", "hello", at, "")
	if err != nil || vector.ID != "d3fb61012e8f8d32d75b293c62702a9c4915ab9bf2cbabda9f566ab75ffe4397" {
		t.Fatalf("NIP-01 vector = %s, %v", vector.ID, err)
	}
	chat, err := buzzChatEvent(keys, "channel", "hello", at, "root", "recipient")
	if err != nil || !verifyBuzzEvent(chat) || firstBuzzTag(chat, "h") != "channel" || firstBuzzTag(chat, "e") != "root" {
		t.Fatalf("chat event = %#v, %v", chat, err)
	}
	edit, err := buzzEditEvent(keys, "channel", chat.ID, "updated", at)
	if err != nil || !verifyBuzzEvent(edit) || edit.Kind != buzzKindEdit {
		t.Fatalf("edit event = %#v, %v", edit, err)
	}
	auth, err := buzzAuthEvent(keys, "wss://relay.example", "challenge", at)
	if err != nil || !verifyBuzzEvent(auth) || firstBuzzTag(auth, "challenge") != "challenge" {
		t.Fatalf("auth event = %#v, %v", auth, err)
	}
	frame, err := buzzFrame("EVENT", chat)
	if err != nil || !json.Valid(frame) {
		t.Fatalf("frame = %s, %v", frame, err)
	}
	canonical, err := buzzCanonicalJSON([]string{"<tag>", "&"})
	if err != nil || string(canonical) != `["<tag>","&"]` {
		t.Fatalf("canonical JSON = %s, %v", canonical, err)
	}
}

func TestBuzzNostrRejectsInvalidKeysAndForgedEvents(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "abc", strings.Repeat("0", 62), "npub1invalid"} {
		if _, err := parseBuzzPublicKey(value); err == nil {
			t.Fatalf("invalid public key %q accepted", value)
		}
	}
	if _, err := parseBuzzPrivateKey(strings.Repeat("0", 64)); err == nil {
		t.Fatal("zero private key accepted")
	}
	keys, _ := parseBuzzPrivateKey(buzzTestSecret)
	event, _ := buzzChatEvent(keys, "channel", "hello", time.Unix(1_800_000_000, 0), "")
	mutations := []func(*buzzEvent){
		func(value *buzzEvent) { value.Content = "forged" },
		func(value *buzzEvent) { value.ID = "bad" },
		func(value *buzzEvent) { value.Sig = "bad" },
		func(value *buzzEvent) { value.CreatedAt = 0 },
		func(value *buzzEvent) { value.Tags = make([][]string, 129) },
	}
	for index, mutate := range mutations {
		candidate := event
		candidate.Tags = make([][]string, len(event.Tags))
		for tagIndex := range event.Tags {
			candidate.Tags[tagIndex] = append([]string(nil), event.Tags[tagIndex]...)
		}
		mutate(&candidate)
		if verifyBuzzEvent(candidate) {
			t.Fatalf("forged event %d accepted", index)
		}
	}
	oversized := event
	oversized.Content = strings.Repeat("x", 1<<20+1)
	if validBuzzEventShape(oversized) {
		t.Fatal("oversized event accepted")
	}
}

func encodeBuzzTestKey(t *testing.T, prefix, value string) string {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	words, err := bech32.ConvertBits(decoded, 8, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := bech32.Encode(prefix, words)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
