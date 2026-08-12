package channel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/bech32"
)

const (
	buzzKindChat          = 9
	buzzKindAuth          = 22242
	buzzKindChannelMeta   = 39000
	buzzKindEdit          = 40003
	buzzKindMemberAdded   = 44100
	buzzKindMemberRemoved = 44101
)

type buzzKeys struct {
	Secret string
	Public string
}

type buzzEvent struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

func parseBuzzPrivateKey(value string) (buzzKeys, error) {
	secret, err := parseBuzzKey(value, "nsec")
	if err != nil {
		return buzzKeys{}, err
	}
	decoded, _ := hex.DecodeString(secret)
	var scalar btcec.ModNScalar
	if scalar.SetByteSlice(decoded) || scalar.IsZero() {
		return buzzKeys{}, ErrInvalid
	}
	_, public := btcec.PrivKeyFromBytes(decoded)
	return buzzKeys{Secret: secret, Public: hex.EncodeToString(public.SerializeCompressed()[1:])}, nil
}

func parseBuzzPublicKey(value string) (string, error) {
	public, err := parseBuzzKey(value, "npub")
	if err != nil {
		return "", err
	}
	decoded, _ := hex.DecodeString(public)
	if _, err = schnorr.ParsePubKey(decoded); err != nil {
		return "", ErrInvalid
	}
	return public, nil
}

func parseBuzzKey(value, expectedPrefix string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), expectedPrefix+"1") {
		prefix, data, err := decodeBuzzBech32(value)
		if err != nil || prefix != expectedPrefix {
			return "", ErrInvalid
		}
		value = hex.EncodeToString(data)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return "", ErrInvalid
	}
	return strings.ToLower(value), nil
}

func decodeBuzzBech32(value string) (string, []byte, error) {
	prefix, words, err := bech32.DecodeNoLimit(strings.ToLower(value))
	if err != nil {
		return "", nil, err
	}
	data, err := bech32.ConvertBits(words, 5, 8, false)
	if err != nil || len(data) != 32 {
		return "", nil, ErrInvalid
	}
	return prefix, data, nil
}

func newBuzzEvent(keys buzzKeys, kind int, tags [][]string, content string, at time.Time) (buzzEvent, error) {
	event := buzzEvent{PubKey: keys.Public, CreatedAt: at.Unix(), Kind: kind, Tags: tags, Content: content}
	identifier, err := buzzEventID(event)
	if err != nil {
		return buzzEvent{}, err
	}
	secret, _ := hex.DecodeString(keys.Secret)
	private, _ := btcec.PrivKeyFromBytes(secret)
	digest, _ := hex.DecodeString(identifier)
	signature, err := schnorr.Sign(private, digest, schnorr.FastSign())
	if err != nil {
		return buzzEvent{}, fmt.Errorf("sign Buzz event: %w", err)
	}
	event.ID, event.Sig = identifier, hex.EncodeToString(signature.Serialize())
	return event, nil
}

func buzzEventID(event buzzEvent) (string, error) {
	canonical, err := buzzCanonicalJSON([]any{0, event.PubKey, event.CreatedAt, event.Kind, event.Tags, event.Content})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func buzzCanonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func verifyBuzzEvent(event buzzEvent) bool {
	if !validBuzzEventShape(event) {
		return false
	}
	identifier, err := buzzEventID(event)
	if err != nil || identifier != event.ID {
		return false
	}
	publicBytes, err := hex.DecodeString(event.PubKey)
	if err != nil {
		return false
	}
	public, err := schnorr.ParsePubKey(publicBytes)
	if err != nil {
		return false
	}
	signatureBytes, err := hex.DecodeString(event.Sig)
	if err != nil {
		return false
	}
	signature, err := schnorr.ParseSignature(signatureBytes)
	if err != nil {
		return false
	}
	digest, _ := hex.DecodeString(identifier)
	return signature.Verify(digest, public)
}

func validBuzzEventShape(event buzzEvent) bool {
	if len(event.ID) != 64 || len(event.PubKey) != 64 || len(event.Sig) != 128 || event.CreatedAt <= 0 ||
		len(event.Content) > 1<<20 || len(event.Tags) > 128 {
		return false
	}
	for _, tag := range event.Tags {
		if len(tag) == 0 || len(tag) > 16 {
			return false
		}
		for _, value := range tag {
			if len(value) > 4096 {
				return false
			}
		}
	}
	return true
}

func buzzAuthEvent(keys buzzKeys, relayURL, challenge string, at time.Time) (buzzEvent, error) {
	return newBuzzEvent(keys, buzzKindAuth, [][]string{{"relay", relayURL}, {"challenge", challenge}}, "", at)
}

func buzzChatEvent(keys buzzKeys, channelID, content string, at time.Time, replyTo string, mentions ...string) (buzzEvent, error) {
	tags := [][]string{{"h", channelID}}
	if replyTo != "" {
		tags = append(tags, []string{"e", replyTo})
	}
	for _, mention := range mentions {
		if mention != "" {
			tags = append(tags, []string{"p", mention})
		}
	}
	return newBuzzEvent(keys, buzzKindChat, tags, content, at)
}

func buzzEditEvent(keys buzzKeys, channelID, targetID, content string, at time.Time) (buzzEvent, error) {
	return newBuzzEvent(keys, buzzKindEdit, [][]string{{"h", channelID}, {"e", targetID}}, content, at)
}

func buzzReplyEvent(keys buzzKeys, channelID, content string, at time.Time, replyTo, requester, marker string) (buzzEvent, error) {
	tags := [][]string{{"h", channelID}}
	if replyTo != "" {
		tags = append(tags, []string{"e", replyTo})
	}
	if requester != "" {
		tags = append(tags, []string{"p", requester})
	}
	if marker != "" {
		tags = append(tags, []string{"client", "gofer", marker})
	}
	return newBuzzEvent(keys, buzzKindChat, tags, content, at)
}

func buzzTagValues(event buzzEvent, name string) []string {
	values := make([]string, 0)
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == name {
			values = append(values, tag[1])
		}
	}
	return values
}

func buzzFrame(parts ...any) ([]byte, error) {
	payload, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	if len(payload) > providerSocketReadLimit {
		return nil, errors.New("Buzz frame exceeds provider limit")
	}
	return payload, nil
}
