package channel

import (
	"strings"
	"unicode"
)

// CommandName identifies one channel control command.
type CommandName string

const (
	// CommandBootstrap starts an agent turn with bootstrap context.
	CommandBootstrap CommandName = "bootstrap"
	// CommandGoal reads, clears, or replaces the conversation goal.
	CommandGoal CommandName = "goal"
	// CommandNew starts a fresh conversation for the current channel topic.
	CommandNew CommandName = "new"
	// CommandStatus reports the active conversation.
	CommandStatus CommandName = "status"
	// CommandModels lists configured model aliases.
	CommandModels CommandName = "models"
	// CommandMemory reports the caller's memory status.
	CommandMemory CommandName = "memory"
	// CommandHelp lists the channel command surface.
	CommandHelp CommandName = "help"
)

var knownCommands = map[string]CommandName{
	"/bootstrap": CommandBootstrap,
	"/goal":      CommandGoal,
	"/new":       CommandNew,
	"/status":    CommandStatus,
	"/models":    CommandModels,
	"/memory":    CommandMemory,
	"/help":      CommandHelp,
}

// Command is one parsed, position-sensitive channel control command.
type Command struct {
	Name CommandName
	Args string
}

// ParseCommand recognizes only the shared command set at byte position zero.
func ParseCommand(text string) (Command, bool) {
	text = StripLeadingMentions(text)
	if !strings.HasPrefix(text, "/") {
		return Command{}, false
	}
	token, arguments := text, ""
	if end := strings.IndexFunc(text, unicode.IsSpace); end >= 0 {
		token, arguments = text[:end], text[end:]
	}
	lookup := strings.ToLower(token)
	if at := strings.IndexByte(lookup, '@'); at > 0 {
		lookup = lookup[:at]
	}
	name, known := knownCommands[lookup]
	if !known {
		return Command{}, false
	}
	return Command{Name: name, Args: strings.TrimSpace(arguments)}, true
}

// StripLeadingMentions removes platform mention tokens only when they begin at
// byte position zero. It leaves ordinary prose and leading-whitespace input
// unchanged.
func StripLeadingMentions(text string) string {
	if strings.TrimLeftFunc(text, unicode.IsSpace) != text {
		return text
	}
	remainder := text
	for remainder != "" {
		end := strings.IndexFunc(remainder, unicode.IsSpace)
		token := remainder
		if end >= 0 {
			token = remainder[:end]
		}
		if !leadingMention(token) {
			break
		}
		if end < 0 {
			return ""
		}
		remainder = strings.TrimLeftFunc(remainder[end:], unicode.IsSpace)
	}
	return remainder
}

// ParseConnectCommand extracts the one-time code from a provider binding
// command. The boolean remains true for a malformed attempt so callers can
// answer it without routing the text to an agent.
func ParseConnectCommand(text, provider string) (string, bool) {
	fields := strings.Fields(text)
	for len(fields) > 0 && leadingMention(fields[0]) {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return "", false
	}
	command := strings.ToLower(fields[0])
	if at := strings.IndexByte(command, '@'); at > 0 {
		command = command[:at]
	}
	wanted := command == "/connect" || normalizeProvider(provider) == TelegramProvider && command == "/start"
	if !wanted {
		return "", false
	}
	if len(fields) < 2 || len(fields[1]) > 128 {
		return "", true
	}
	return fields[1], true
}

// ConnectableProvider reports whether a provider supports user-issued
// one-time binding codes.
func ConnectableProvider(provider string) bool {
	switch normalizeProvider(provider) {
	case SlackProvider, TelegramProvider, DiscordProvider, FeishuProvider,
		DingTalkProvider, WeComProvider, WeChatProvider, BuzzProvider:
		return true
	default:
		return false
	}
}

func leadingMention(token string) bool {
	return len(token) > 1 && strings.HasPrefix(token, "@") ||
		strings.HasPrefix(token, "<@") && strings.HasSuffix(token, ">")
}
