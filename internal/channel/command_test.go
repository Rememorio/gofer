package channel

import "testing"

func TestParseCommand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		text string
		name CommandName
		args string
		ok   bool
	}{
		{text: "/new", name: CommandNew, ok: true},
		{text: "/GOAL  ship it ", name: CommandGoal, args: "ship it", ok: true},
		{text: "/bootstrap", name: CommandBootstrap, ok: true},
		{text: "/new@GoferBot", name: CommandNew, ok: true},
		{text: "@bot <@U1>\t/help", name: CommandHelp, ok: true},
		{text: "/unknown", ok: false},
		{text: " /new", ok: false},
		{text: " @bot /new", ok: false},
		{text: "/newer", ok: false},
	} {
		got, ok := ParseCommand(test.text)
		if ok != test.ok || got.Name != test.name || got.Args != test.args {
			t.Errorf("ParseCommand(%q) = %#v, %v", test.text, got, ok)
		}
	}
}

func TestStripLeadingMentions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		text string
		want string
	}{
		{text: "@bot <@U1> /goal ship", want: "/goal ship"},
		{text: "plain", want: "plain"},
		{text: " @bot /goal", want: " @bot /goal"},
		{text: "@bot", want: ""},
	} {
		if got := StripLeadingMentions(test.text); got != test.want {
			t.Fatalf("StripLeadingMentions(%q) = %q, want %q", test.text, got, test.want)
		}
	}
}

func TestParseConnectCommand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		text     string
		provider string
		code     string
		attempt  bool
	}{
		{text: "/connect abc", provider: "slack", code: "abc", attempt: true},
		{text: "@bot <@U1> /CONNECT abc extra", provider: "feishu", code: "abc", attempt: true},
		{text: "/start token", provider: "telegram", code: "token", attempt: true},
		{text: "/start@gofer token", provider: "telegram", code: "token", attempt: true},
		{text: "/connect", provider: "buzz", attempt: true},
		{text: "/start token", provider: "slack", attempt: false},
		{text: "hello /connect abc", provider: "slack", attempt: false},
	} {
		code, attempted := ParseConnectCommand(test.text, test.provider)
		if code != test.code || attempted != test.attempt {
			t.Errorf("ParseConnectCommand(%q, %q) = %q, %v", test.text, test.provider, code, attempted)
		}
	}
}
