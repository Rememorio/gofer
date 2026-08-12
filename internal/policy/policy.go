package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

var (
	// ErrInvalidPolicy identifies malformed rules, descriptors, or actions.
	ErrInvalidPolicy = errors.New("invalid policy")
	// ErrDenied identifies an action rejected by policy.
	ErrDenied = errors.New("action denied by policy")
	// ErrApprovalRequired identifies an action requiring an external approval.
	ErrApprovalRequired = errors.New("action requires approval")
)

// Effect identifies the side effect class of an action.
type Effect string

// Supported policy effects.
const (
	EffectRead        Effect = "read"
	EffectWrite       Effect = "write"
	EffectExecute     Effect = "execute"
	EffectNetwork     Effect = "network"
	EffectDestructive Effect = "destructive"
)

// Decision is the result attached to a matching rule.
type Decision string

// Supported rule decisions.
const (
	DecisionAllow           Decision = "allow"
	DecisionDeny            Decision = "deny"
	DecisionRequireApproval Decision = "require_approval"
)

// Action is one model-requested operation to authorize.
type Action struct {
	Tool     string `json:"tool"`
	Effect   Effect `json:"effect"`
	Resource string `json:"resource,omitempty"`
}

// Authorizer decides whether an action may proceed.
type Authorizer interface {
	Authorize(context.Context, Action) error
}

// Rule matches tools, effects, and resources using stable glob patterns.
// Empty patterns match every value.
type Rule struct {
	Tool     string
	Effect   Effect
	Resource string
	Decision Decision
}

// Static is an immutable, ordered, first-match policy.
type Static struct {
	defaultDecision Decision
	rules           []Rule
}

// NewStatic validates rules and constructs a first-match policy.
func NewStatic(defaultDecision Decision, rules ...Rule) (*Static, error) {
	if !defaultDecision.valid() {
		return nil, fmt.Errorf("%w: invalid default decision %q", ErrInvalidPolicy, defaultDecision)
	}
	copyRules := append([]Rule(nil), rules...)
	for index, rule := range copyRules {
		if !rule.Decision.valid() || (rule.Effect != "" && !rule.Effect.valid()) {
			return nil, fmt.Errorf("%w: invalid rule %d", ErrInvalidPolicy, index)
		}
		for _, pattern := range []string{rule.Tool, rule.Resource} {
			if pattern == "" || strings.HasSuffix(pattern, "/**") {
				continue
			}
			if _, err := path.Match(pattern, "probe"); err != nil {
				return nil, fmt.Errorf("%w: rule %d pattern: %w", ErrInvalidPolicy, index, err)
			}
		}
	}
	return &Static{defaultDecision: defaultDecision, rules: copyRules}, nil
}

// Authorize returns nil only when the first matching decision allows action.
func (policy *Static) Authorize(ctx context.Context, action Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if policy == nil {
		return fmt.Errorf("%w: static policy is nil", ErrInvalidPolicy)
	}
	if err := action.validate(); err != nil {
		return err
	}
	decision := policy.defaultDecision
	for _, rule := range policy.rules {
		if rule.matches(action) {
			decision = rule.Decision
			break
		}
	}
	return decisionError(decision, action)
}

// Descriptor maps a tool's arguments to policy resources.
type Descriptor struct {
	Effect         Effect
	ResourceFields []string
}

// Middleware enforces an Authorizer before tool execution.
type Middleware struct {
	authorizer  Authorizer
	descriptors map[string]Descriptor
}

// NewMiddleware validates and constructs policy middleware.
func NewMiddleware(authorizer Authorizer, descriptors map[string]Descriptor) (*Middleware, error) {
	if authorizer == nil {
		return nil, fmt.Errorf("%w: authorizer is required", ErrInvalidPolicy)
	}
	copyDescriptors := make(map[string]Descriptor, len(descriptors))
	for name, descriptor := range descriptors {
		if strings.TrimSpace(name) == "" || !descriptor.Effect.valid() {
			return nil, fmt.Errorf("%w: invalid descriptor for %q", ErrInvalidPolicy, name)
		}
		for _, field := range descriptor.ResourceFields {
			if strings.TrimSpace(field) == "" {
				return nil, fmt.Errorf("%w: resource field is empty", ErrInvalidPolicy)
			}
		}
		descriptor.ResourceFields = append([]string(nil), descriptor.ResourceFields...)
		copyDescriptors[name] = descriptor
	}
	return &Middleware{authorizer: authorizer, descriptors: copyDescriptors}, nil
}

// BeforeModel leaves model requests unchanged.
func (*Middleware) BeforeModel(context.Context, *model.Request) error { return nil }

// AfterModel accepts normalized model responses.
func (*Middleware) AfterModel(context.Context, model.Response) error { return nil }

// BeforeTool authorizes every resource referenced by a tool call.
func (middleware *Middleware) BeforeTool(ctx context.Context, call domain.ToolCall) error {
	if middleware == nil || middleware.authorizer == nil {
		return fmt.Errorf("%w: middleware is not configured", ErrInvalidPolicy)
	}
	descriptor, exists := middleware.descriptors[call.Name]
	if !exists {
		return middleware.authorizer.Authorize(ctx, Action{Tool: call.Name, Effect: EffectExecute})
	}
	resources, err := resourcesFromArguments(call.Arguments, descriptor.ResourceFields)
	if err != nil {
		return fmt.Errorf("%w: tool %s: %w", ErrInvalidPolicy, call.Name, err)
	}
	if len(resources) == 0 {
		resources = append(resources, "")
	}
	for _, resource := range resources {
		if err := middleware.authorizer.Authorize(ctx, Action{
			Tool: call.Name, Effect: descriptor.Effect, Resource: resource,
		}); err != nil {
			return err
		}
	}
	return nil
}

// AfterTool accepts tool outcomes without changing them.
func (*Middleware) AfterTool(context.Context, domain.ToolCall, domain.ToolResult) error { return nil }

func resourcesFromArguments(arguments json.RawMessage, fields []string) ([]string, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	var values map[string]any
	if err := json.Unmarshal(arguments, &values); err != nil {
		return nil, fmt.Errorf("decode arguments: %w", err)
	}
	resources := make([]string, 0)
	for _, field := range fields {
		value, exists := values[field]
		if !exists {
			return nil, fmt.Errorf("resource field %q is missing", field)
		}
		switch typed := value.(type) {
		case string:
			resources = append(resources, typed)
		case []any:
			for _, item := range typed {
				text, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("resource field %q contains a non-string value", field)
				}
				resources = append(resources, text)
			}
		default:
			return nil, fmt.Errorf("resource field %q must be a string or string array", field)
		}
	}
	return resources, nil
}

func (action Action) validate() error {
	if strings.TrimSpace(action.Tool) == "" || !action.Effect.valid() {
		return fmt.Errorf("%w: action tool and effect are required", ErrInvalidPolicy)
	}
	return nil
}

func (rule Rule) matches(action Action) bool {
	return (rule.Effect == "" || rule.Effect == action.Effect) &&
		wildcardMatch(rule.Tool, action.Tool) && wildcardMatch(rule.Resource, action.Resource)
}

func wildcardMatch(pattern, value string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if prefix, found := strings.CutSuffix(pattern, "/**"); found {
		return value == prefix || strings.HasPrefix(value, prefix+"/")
	}
	matched, _ := path.Match(pattern, value)
	return matched
}

func decisionError(decision Decision, action Action) error {
	switch decision {
	case DecisionAllow:
		return nil
	case DecisionDeny:
		return fmt.Errorf("%w: %s %s via %s", ErrDenied, action.Effect, action.Resource, action.Tool)
	case DecisionRequireApproval:
		return fmt.Errorf("%w: %s %s via %s", ErrApprovalRequired, action.Effect, action.Resource, action.Tool)
	default:
		return fmt.Errorf("%w: unknown decision %q", ErrInvalidPolicy, decision)
	}
}

func (effect Effect) valid() bool {
	switch effect {
	case EffectRead, EffectWrite, EffectExecute, EffectNetwork, EffectDestructive:
		return true
	default:
		return false
	}
}

func (decision Decision) valid() bool {
	switch decision {
	case DecisionAllow, DecisionDeny, DecisionRequireApproval:
		return true
	default:
		return false
	}
}
