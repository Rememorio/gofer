package uploads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const inputPlaceholder = "{input}"

// CommandConverter runs a configured argv directly, never through a shell.
// The command must write Markdown to stdout and contain an {input} placeholder.
type CommandConverter struct {
	command  []string
	maxBytes int64
}

// NewCommandConverter validates and copies a converter command.
func NewCommandConverter(command []string, maxBytes int64) (*CommandConverter, error) {
	if len(command) == 0 || maxBytes < 1024 || maxBytes > 100<<20 {
		return nil, ErrInvalidConfig
	}
	foundInput := false
	for _, argument := range command {
		if strings.TrimSpace(argument) == "" || strings.IndexByte(argument, 0) >= 0 {
			return nil, fmt.Errorf("%w: converter arguments cannot be blank or contain NUL", ErrInvalidConfig)
		}
		foundInput = foundInput || strings.Contains(argument, inputPlaceholder)
	}
	if !foundInput {
		return nil, fmt.Errorf("%w: converter command requires %s", ErrInvalidConfig, inputPlaceholder)
	}
	return &CommandConverter{command: append([]string(nil), command...), maxBytes: maxBytes}, nil
}

// Convert writes the input to a private temporary directory, invokes the
// configured argv, and returns bounded stdout.
func (converter *CommandConverter) Convert(ctx context.Context, filename string, reader io.Reader) ([]byte, error) {
	if converter == nil || len(converter.command) == 0 || reader == nil || !validFilename(filename) {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "gofer-document-")
	if err != nil {
		return nil, fmt.Errorf("create conversion directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	input := filepath.Join(directory, "input"+strings.ToLower(filepath.Ext(filename)))
	file, err := os.OpenFile(input, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create conversion input: %w", err)
	}
	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return nil, fmt.Errorf("stage conversion input: %w", errors.Join(copyErr, closeErr))
	}
	argv := replaceInput(converter.command, input)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = directory
	command.Env = converterEnvironment(directory)
	stderr := &boundedBuffer{remaining: 4096}
	command.Stderr = stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: open converter output: %w", ErrConversion, err)
	}
	if err = command.Start(); err != nil {
		return nil, fmt.Errorf("%w: start converter command: %w", ErrConversion, err)
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, converter.maxBytes+1))
	tooLarge := int64(len(output)) > converter.maxBytes
	if readErr != nil || tooLarge {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if tooLarge {
		return nil, fmt.Errorf("%w: output exceeds %d bytes", ErrConversion, converter.maxBytes)
	}
	if readErr != nil {
		return nil, fmt.Errorf("%w: read converter output: %w", ErrConversion, readErr)
	}
	if waitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: converter command: %w", ErrConversion, waitErr)
	}
	return output, nil
}

func converterEnvironment(directory string) []string {
	environment := []string{
		"HOME=" + directory, "TMPDIR=" + directory, "TMP=" + directory, "TEMP=" + directory,
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8",
	}
	for _, name := range []string{"PATH", "SystemRoot", "ComSpec", "PATHEXT"} {
		if value := os.Getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func replaceInput(command []string, input string) []string {
	result := make([]string, len(command))
	for index, argument := range command {
		result[index] = strings.ReplaceAll(argument, inputPlaceholder, input)
	}
	return result
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int64
	exceeded  bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	if int64(len(data)) <= buffer.remaining {
		written, err := buffer.buffer.Write(data)
		buffer.remaining -= int64(written)
		return written, err
	}
	allowed := max(buffer.remaining, 0)
	if allowed > 0 {
		_, _ = buffer.buffer.Write(data[:allowed])
	}
	buffer.remaining = 0
	buffer.exceeded = true
	return len(data), nil
}

func (buffer *boundedBuffer) String() string { return buffer.buffer.String() }
