package uploads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCommandConverter(t *testing.T) {
	converter := commandTestConverter(t, 1024, "success")
	output, err := converter.Convert(context.Background(), "report.PDF", strings.NewReader("document"))
	if err != nil || string(output) != "# converted\ndocument" {
		t.Fatalf("Convert() = %q, %v", output, err)
	}
}

func TestCommandConverterFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		context func() (context.Context, context.CancelFunc)
	}{
		{name: "exit", mode: "fail", context: backgroundContext},
		{name: "too large", mode: "large", context: backgroundContext},
		{name: "timeout", mode: "block", context: shortContext},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.context()
			defer cancel()
			_, err := commandTestConverter(t, 1024, test.mode).Convert(ctx, "input.docx", strings.NewReader("x"))
			if err == nil {
				t.Fatal("Convert() error = nil")
			}
			if test.mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout error = %v", err)
			}
		})
	}
}

func TestCommandConverterValidationAndBuffer(t *testing.T) {
	t.Parallel()
	invalid := [][]string{nil, {""}, {"converter", "input"}, {"converter", "bad\x00{input}"}}
	for _, command := range invalid {
		if _, err := NewCommandConverter(command, 1024); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewCommandConverter(%q) error = %v", command, err)
		}
	}
	if _, err := NewCommandConverter([]string{"converter", "{input}"}, 1); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("small limit error = %v", err)
	}
	converter := &CommandConverter{}
	if _, err := converter.Convert(context.Background(), "x.pdf", strings.NewReader("x")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid converter error = %v", err)
	}
	valid := commandTestConverter(t, 1024, "success")
	if _, err := valid.Convert(context.Background(), "../x.pdf", strings.NewReader("x")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unsafe filename error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := valid.Convert(cancelled, "x.pdf", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	buffer := &boundedBuffer{remaining: 3}
	if written, err := buffer.Write([]byte("ab")); err != nil || written != 2 {
		t.Fatalf("bounded write = %d, %v", written, err)
	}
	if written, err := buffer.Write([]byte("cdef")); err != nil || written != 4 || !buffer.exceeded || buffer.String() != "abc" {
		t.Fatalf("overflow write = %d, %v, %#v", written, err, buffer)
	}
	if got := replaceInput([]string{"cmd", "--file={input}"}, "input.file"); got[1] != "--file=input.file" {
		t.Fatalf("replaceInput() = %#v", got)
	}
}

func TestCommandConverterHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	mode := os.Args[len(os.Args)-2]
	input := os.Args[len(os.Args)-1]
	switch mode {
	case "success":
		data, _ := os.ReadFile(input)
		_, _ = fmt.Fprintf(os.Stdout, "# converted\n%s", data)
	case "large":
		_, _ = io.CopyN(os.Stdout, bytes.NewReader(bytes.Repeat([]byte("x"), 2048)), 2048)
	case "block":
		time.Sleep(time.Second)
	case "fail":
		_, _ = fmt.Fprint(os.Stderr, "conversion failed")
		os.Exit(2)
	}
	os.Exit(0)
}

func commandTestConverter(t *testing.T, maxBytes int64, mode string) *CommandConverter {
	t.Helper()
	converter, err := NewCommandConverter([]string{os.Args[0], "-test.run=TestCommandConverterHelper", "--", mode, "{input}"}, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return converter
}

func backgroundContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func shortContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Millisecond)
}
