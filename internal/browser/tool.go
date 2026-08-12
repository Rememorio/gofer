package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/artifact"
	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
	"github.com/Rememorio/gofer/internal/workspace"
)

var screenshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Tools binds one thread key, workspace, and artifact catalog to browser tools.
type Tools struct {
	Manager   *Manager
	Key       string
	Workspace *workspace.Thread
	Artifacts *artifact.Catalog
	Now       func() time.Time
	Random    io.Reader
}

// Register validates dependencies and atomically registers browser tools.
func (tools Tools) Register(registry *tool.Registry) error {
	if registry == nil || tools.Manager == nil || strings.TrimSpace(tools.Key) == "" ||
		tools.Workspace == nil || tools.Artifacts == nil {
		return fmt.Errorf("%w: registry, manager, key, workspace, and artifacts are required", ErrInvalidConfig)
	}
	if tools.Now == nil {
		tools.Now = time.Now
	}
	if tools.Random == nil {
		tools.Random = rand.Reader
	}
	return registry.RegisterAll(tools.definitions()...)
}

// PolicyDescriptors returns authorization metadata for browser tools.
func PolicyDescriptors() map[string]policy.Descriptor {
	return map[string]policy.Descriptor{
		"browser_navigate":   {Effect: policy.EffectNetwork, ResourceFields: []string{"url"}},
		"browser_snapshot":   {Effect: policy.EffectRead},
		"browser_click":      {Effect: policy.EffectExecute},
		"browser_type":       {Effect: policy.EffectExecute},
		"browser_scroll":     {Effect: policy.EffectRead},
		"browser_back":       {Effect: policy.EffectNetwork},
		"browser_screenshot": {Effect: policy.EffectWrite},
		"browser_close":      {Effect: policy.EffectExecute},
	}
}

func (tools Tools) definitions() []tool.Tool {
	return []tool.Tool{
		tools.snapshotTool("browser_navigate", "Navigate to a public HTTP(S) URL and return an interactive snapshot.",
			`{"type":"object","properties":{"url":{"type":"string","minLength":1}},"required":["url"],"additionalProperties":false}`,
			func(ctx context.Context, session Session, arguments json.RawMessage) (Snapshot, error) {
				var input struct {
					URL string `json:"url"`
				}
				if err := decodeArguments(arguments, &input); err != nil {
					return Snapshot{}, err
				}
				return session.Navigate(ctx, input.URL)
			}),
		tools.snapshotTool("browser_snapshot", "Refresh the current page snapshot and element references.", emptySchema,
			func(ctx context.Context, session Session, _ json.RawMessage) (Snapshot, error) {
				return session.Snapshot(ctx)
			}),
		tools.snapshotTool("browser_click", "Click an element ref from the latest browser snapshot.",
			`{"type":"object","properties":{"ref":{"type":"integer","minimum":1}},"required":["ref"],"additionalProperties":false}`,
			func(ctx context.Context, session Session, arguments json.RawMessage) (Snapshot, error) {
				var input struct {
					Ref int `json:"ref"`
				}
				if err := decodeArguments(arguments, &input); err != nil {
					return Snapshot{}, err
				}
				return session.Click(ctx, input.Ref)
			}),
		tools.typeTool(), tools.scrollTool(),
		tools.snapshotTool("browser_back", "Navigate back in browser history.", emptySchema,
			func(ctx context.Context, session Session, _ json.RawMessage) (Snapshot, error) {
				return session.Back(ctx)
			}),
		tools.screenshotTool(), tools.closeTool(),
	}
}

const emptySchema = `{"type":"object","additionalProperties":false}`

func (tools Tools) typeTool() tool.Tool {
	return tools.snapshotTool("browser_type", "Fill an input element ref and optionally submit it.",
		`{"type":"object","properties":{"ref":{"type":"integer","minimum":1},"text":{"type":"string","maxLength":100000},"submit":{"type":"boolean"}},"required":["ref","text"],"additionalProperties":false}`,
		func(ctx context.Context, session Session, arguments json.RawMessage) (Snapshot, error) {
			var input struct {
				Ref    int    `json:"ref"`
				Text   string `json:"text"`
				Submit bool   `json:"submit"`
			}
			if err := decodeArguments(arguments, &input); err != nil {
				return Snapshot{}, err
			}
			return session.Type(ctx, input.Ref, input.Text, input.Submit)
		})
}

func (tools Tools) scrollTool() tool.Tool {
	return tools.snapshotTool("browser_scroll", "Scroll the current page by bounded pixel deltas.",
		`{"type":"object","properties":{"delta_x":{"type":"integer","minimum":-100000,"maximum":100000},"delta_y":{"type":"integer","minimum":-100000,"maximum":100000}},"additionalProperties":false}`,
		func(ctx context.Context, session Session, arguments json.RawMessage) (Snapshot, error) {
			var input struct {
				DeltaX int `json:"delta_x"`
				DeltaY int `json:"delta_y"`
			}
			if err := decodeArguments(arguments, &input); err != nil {
				return Snapshot{}, err
			}
			return session.Scroll(ctx, input.DeltaX, input.DeltaY)
		})
}

func (tools Tools) snapshotTool(
	name, description, schema string,
	execute func(context.Context, Session, json.RawMessage) (Snapshot, error),
) tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name: name, Description: description, InputSchema: json.RawMessage(schema),
			ReadOnly: name == "browser_snapshot" || name == "browser_scroll", UntrustedOutput: true,
		},
		ExecuteFunc: func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			lease, err := tools.Manager.Acquire(ctx, tools.Key)
			if err != nil {
				return nil, err
			}
			defer lease.Release()
			snapshot, err := execute(ctx, lease.Session, arguments)
			if err != nil {
				return nil, err
			}
			return json.Marshal(struct {
				Snapshot Snapshot `json:"snapshot"`
				Rendered string   `json:"rendered"`
			}{Snapshot: snapshot, Rendered: snapshot.Render()})
		},
	}
}

func (tools Tools) screenshotTool() tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name: "browser_screenshot", Description: "Capture the current browser page as a PNG output artifact.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string","minLength":1,"maxLength":100},"full_page":{"type":"boolean"}},"additionalProperties":false}`),
		},
		ExecuteFunc: func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			var input struct {
				Filename string `json:"filename"`
				FullPage bool   `json:"full_page"`
			}
			if err := decodeArguments(arguments, &input); err != nil {
				return nil, err
			}
			lease, err := tools.Manager.Acquire(ctx, tools.Key)
			if err != nil {
				return nil, err
			}
			defer lease.Release()
			image, err := lease.Session.Screenshot(ctx, input.FullPage)
			if err != nil {
				return nil, err
			}
			virtualPath, err := tools.writeScreenshot(input.Filename, image)
			if err != nil {
				return nil, err
			}
			artifacts, err := tools.Artifacts.Present(ctx, tools.Workspace, []string{virtualPath}, tools.Now())
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"path": virtualPath, "artifacts": artifacts})
		},
	}
}

func (tools Tools) closeTool() tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name: "browser_close", Description: "Close and discard the current thread browser session.",
			InputSchema: json.RawMessage(emptySchema),
		},
		ExecuteFunc: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			err := tools.Manager.CloseSession(tools.Key)
			if errors.Is(err, ErrNotFound) {
				return json.RawMessage(`{"closed":false}`), nil
			}
			if err != nil {
				return nil, err
			}
			return json.RawMessage(`{"closed":true}`), nil
		},
	}
}

func (tools Tools) writeScreenshot(requested string, image []byte) (string, error) {
	stem := strings.TrimSuffix(requested, path.Ext(requested))
	if requested == "" {
		stem = "browser-" + tools.Now().UTC().Format("20060102-150405")
	}
	if !screenshotNamePattern.MatchString(stem) {
		return "", errors.New("screenshot filename must contain only letters, digits, dots, underscores, or hyphens")
	}
	for attempt := 0; attempt < 100; attempt++ {
		suffix, err := randomSuffix(tools.Random)
		if err != nil {
			return "", err
		}
		virtualPath := workspace.OutputsRoot + "/" + stem + "-" + suffix + ".png"
		if err := tools.Workspace.CreateOutput(virtualPath, image); err == nil {
			return virtualPath, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("allocate screenshot filename: collision limit exceeded")
}

func randomSuffix(reader io.Reader) (string, error) {
	data := make([]byte, 4)
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func decodeArguments(arguments json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode browser arguments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode browser arguments: multiple JSON values")
	}
	return nil
}
