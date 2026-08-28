package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invopop/jsonschema"

	"github.com/ccheers/codexadkv2/codex/protocol"
)

// DynamicTool is one ad hoc tool exposed to the model for the lifetime of a
// session, via the app-server's experimental thread/start.dynamicTools feature.
//
// A session dispatches calls to the tool's Call method automatically and answers
// the server with the result, so callers do not have to watch for tool-call
// requests and reply to them by hand. Build instances with NewTool.
//
// There is deliberately no Namespace method. Which group a tool belongs to is an
// assembly decision, not something the tool knows about itself: the same tool can
// be registered standalone in one session and inside a group in another, and
// baking a namespace into the tool would prevent that.
type DynamicTool interface {
	// Name is the tool name the model calls.
	Name() string

	// Description is shown to the model. It is the only thing the model has to
	// decide whether to call the tool, so it should say what the tool does and
	// when to reach for it.
	Description() string

	// InputSchema is the JSON Schema describing the tool's arguments.
	InputSchema() map[string]any

	// Call executes the tool with the model's arguments, which are JSON matching
	// InputSchema, and returns the textual result. A non-nil error is reported to
	// the model as a failed tool call.
	//
	// callID is the identifier the server assigned to this invocation, useful for
	// correlating logs across the item/started, call, and item/completed stages.
	//
	// The context is the one the session was opened with, so a cancelled session
	// cancels in-flight tool calls. Calls run on their own goroutine and may block
	// as long as the work genuinely takes; the turn is waiting on the answer.
	Call(ctx context.Context, callID string, args json.RawMessage) (string, error)
}

// ToolGroup is a set of related tools presented to the model under one name.
//
// Grouping exists for progressive disclosure: the model reads the group's
// Description first to decide whether the area is relevant, and only then looks
// at the individual tools' schemas. That makes Description load-bearing rather
// than decorative, which is why it belongs to the group and cannot be derived
// from the tools inside it.
type ToolGroup struct {
	// Name is the namespace the model addresses the group by.
	Name string

	// Description is the group's own description, and carries the whole weight of
	// the first hop in progressive disclosure. Describe the area the group covers,
	// not the individual tools.
	Description string

	// ToolDeferLoading mark the tool use deferLoading
	ToolDeferLoading bool

	// Tools are the functions nested under this group.
	Tools []DynamicTool
}

// NewTool builds a DynamicTool from a typed handler.
//
// The input schema is reflected from the Args type: `json:"..."` tags set the wire
// names and `jsonschema:"..."` tags add descriptions. The handler receives decoded
// arguments:
//
//	type GrepArgs struct {
//	    Pattern string `json:"pattern" jsonschema:"description=regular expression to match"`
//	    Path    string `json:"path,omitempty" jsonschema:"description=directory to search, defaults to cwd"`
//	}
//
// # Which arguments the model must supply
//
// Reflection treats a field as REQUIRED unless its json tag says `omitempty`.
// That is the opposite of what most people expect, and it matters: a field the
// model believes is mandatory changes how it calls the tool. So mark genuinely
// optional arguments with `omitempty`:
//
//	Pattern string `json:"pattern"`            // required
//	Path    string `json:"path,omitempty"`     // optional
//
// A `jsonschema:"required"` tag forces required even with omitempty, for the rare
// case where the two need to disagree.
//
// Descriptions are worth writing. They are all the model has to decide what to
// pass, and a tool with well-described arguments gets called correctly far more
// often than one relying on parameter names alone.
//
//	grep := codex.NewTool("grep", "Search files in the repo by regular expression",
//	    func(ctx context.Context, callID string, args GrepArgs) (string, error) {
//	        return runGrep(ctx, args.Pattern, args.Path)
//	    })
//
// Returning a non-nil error from the handler reports a failed tool call to the
// model, with the error text as the reason. That is usually better than returning
// an error string as the result, because the model can tell the difference
// between "the tool failed" and "the tool succeeded and here is the answer".
func NewTool[Args any](
	name, description string,
	handle func(ctx context.Context, callID string, args Args) (string, error),
) DynamicTool {
	return &typedTool[Args]{
		name:        name,
		description: description,
		schema:      reflectToolSchema[Args](),
		handle:      handle,
	}
}

// typedTool is the handler-backed DynamicTool built by NewTool.
type typedTool[Args any] struct {
	name        string
	description string
	schema      map[string]any
	handle      func(ctx context.Context, callID string, args Args) (string, error)
}

func (t *typedTool[Args]) Name() string                { return t.name }
func (t *typedTool[Args]) Description() string         { return t.description }
func (t *typedTool[Args]) InputSchema() map[string]any { return t.schema }

func (t *typedTool[Args]) Call(ctx context.Context, callID string, args json.RawMessage) (string, error) {
	var decoded Args
	// An empty payload is a tool with no arguments, which is legitimate; decoding
	// "" would fail where decoding nothing should succeed.
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &decoded); err != nil {
			return "", fmt.Errorf("codex: decoding arguments for tool %s: %w", t.name, err)
		}
	}
	return t.handle(ctx, callID, decoded)
}

// reflectToolSchema renders the JSON Schema for a tool's Args type.
//
// The schema is inlined rather than using $ref/$defs, and the draft marker is
// stripped, because a model tool definition expects a plain self-contained object
// schema.
func reflectToolSchema[Args any]() map[string]any {
	var zero Args
	reflector := &jsonschema.Reflector{DoNotReference: true}

	raw, err := reflector.Reflect(zero).MarshalJSON()
	if err != nil {
		// A tool with an unusable schema is still better than a failed session:
		// the model sees an argument-free tool rather than nothing at all.
		return map[string]any{"type": "object"}
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return map[string]any{"type": "object"}
	}
	delete(schema, "$schema")
	return schema
}

// deferLoadingInstructions renders the developer-instructions block for groups
// whose tools are defer-loaded. A defer-loaded group's schemas are withheld
// from thread/start, so this prompt is the model's only upfront map of what
// those tools are and how to address them.
func deferLoadingInstructions(groups []ToolGroup) string {
	var b strings.Builder
	for _, group := range groups {
		if !group.ToolDeferLoading {
			continue
		}
		b.WriteString(fmt.Sprintf("### %s\n%s\n", group.Name, group.Description))
		for _, tool := range group.Tools {
			b.WriteString(fmt.Sprintf("- %s__%s\n", group.Name, tool.Name()))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "## ToolGroups:\n" + b.String()
}

// appendDeveloperInstructions appends a block to existing developer
// instructions. The field is a single string on the wire, so "two sources set
// it" resolves by concatenation rather than last-write-wins: the caller's
// prompt and the SDK's defer-loading map are complements, not alternatives.
func appendDeveloperInstructions(existing *string, block string) *string {
	if existing == nil || *existing == "" {
		return &block
	}
	joined := *existing + "\n\n" + block
	return &joined
}

// toolKey identifies a tool for dispatch. A tool inside a group is addressed by
// namespace and name together, since two groups may each define a "query" tool.
func toolKey(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "\x00" + name
}

// toolRegistry maps dispatch keys to tools and holds the spec sent at
// thread/start.
type toolRegistry struct {
	tools map[string]DynamicTool
	specs []*protocol.DynamicToolSpec
}

// buildToolRegistry validates the tools and renders the protocol spec.
//
// Validation happens here rather than at call time because a duplicate or unnamed
// tool is a programming error the caller should hear about when opening the
// session, not when the model happens to invoke it mid-turn.
func buildToolRegistry(singles []DynamicTool, groups []ToolGroup) (*toolRegistry, error) {
	reg := &toolRegistry{tools: make(map[string]DynamicTool)}

	add := func(namespace string, tool DynamicTool) error {
		if tool == nil {
			return fmt.Errorf("codex: nil tool registered under namespace %q", namespace)
		}
		if tool.Name() == "" {
			return fmt.Errorf("codex: tool with an empty name registered under namespace %q", namespace)
		}
		key := toolKey(namespace, tool.Name())
		if _, dup := reg.tools[key]; dup {
			if namespace == "" {
				return fmt.Errorf("codex: duplicate tool %q", tool.Name())
			}
			return fmt.Errorf("codex: duplicate tool %q in group %q", tool.Name(), namespace)
		}
		reg.tools[key] = tool
		return nil
	}

	for _, tool := range singles {
		if err := add("", tool); err != nil {
			return nil, err
		}
		schema, err := marshalSchema(tool)
		if err != nil {
			return nil, err
		}
		spec := protocol.NewDynamicToolSpecFunction(protocol.DynamicToolSpecFunctionPayload{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: schema,
		})
		reg.specs = append(reg.specs, &spec)
	}

	seenGroups := make(map[string]bool, len(groups))
	for _, group := range groups {
		if group.Name == "" {
			return nil, fmt.Errorf("codex: tool group with an empty name")
		}
		if seenGroups[group.Name] {
			return nil, fmt.Errorf("codex: duplicate tool group %q", group.Name)
		}
		seenGroups[group.Name] = true
		if group.Description == "" {
			// The group description is the first hop of progressive disclosure: the
			// model uses it to decide whether to look inside at all.
			return nil, fmt.Errorf(
				"codex: tool group %q has no description; the model needs it to decide "+
					"whether to expand the group", group.Name)
		}
		if len(group.Tools) == 0 {
			return nil, fmt.Errorf("codex: tool group %q has no tools", group.Name)
		}

		nested := make([]*protocol.DynamicToolNamespaceTool, 0, len(group.Tools))
		for _, tool := range group.Tools {
			if err := add(group.Name, tool); err != nil {
				return nil, err
			}
			schema, err := marshalSchema(tool)
			if err != nil {
				return nil, err
			}
			fn := protocol.NewDynamicToolNamespaceToolFunction(
				protocol.DynamicToolNamespaceToolFunctionPayload{
					Name:         tool.Name(),
					Description:  tool.Description(),
					InputSchema:  schema,
					DeferLoading: &group.ToolDeferLoading,
				})
			nested = append(nested, &fn)
		}

		spec := protocol.NewDynamicToolSpecNamespace(protocol.DynamicToolSpecNamespacePayload{
			Name:        group.Name,
			Description: group.Description,
			Tools:       nested,
		})
		reg.specs = append(reg.specs, &spec)
	}
	return reg, nil
}

func marshalSchema(tool DynamicTool) (json.RawMessage, error) {
	schema := tool.InputSchema()
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("codex: encoding input schema for tool %s: %w", tool.Name(), err)
	}
	return raw, nil
}

// lookup resolves a tool call to its handler.
func (r *toolRegistry) lookup(namespace, name string) DynamicTool {
	if r == nil {
		return nil
	}
	return r.tools[toolKey(namespace, name)]
}

// dispatchToolCall runs the tool a call names and shapes the protocol response.
func (r *toolRegistry) dispatchToolCall(
	ctx context.Context,
	p *protocol.DynamicToolCallParams,
) *protocol.DynamicToolCallResponse {
	namespace := ""
	if p.Namespace != nil {
		namespace = *p.Namespace
	}

	tool := r.lookup(namespace, p.Tool)
	if tool == nil {
		// Answer rather than error out: the turn is blocked on a reply, and telling
		// the model the tool does not exist lets it recover.
		return failedToolCall(fmt.Sprintf("unknown tool %q", qualifiedToolName(namespace, p.Tool)))
	}

	content, err := tool.Call(ctx, p.CallID, p.Arguments)
	if err != nil {
		return failedToolCall(err.Error())
	}
	return successfulToolCall(content)
}

func qualifiedToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "." + name
}

func successfulToolCall(text string) *protocol.DynamicToolCallResponse {
	item := protocol.NewDynamicToolCallOutputContentItemInputText(
		protocol.DynamicToolCallOutputContentItemInputTextPayload{Text: text})
	return &protocol.DynamicToolCallResponse{
		Success:      true,
		ContentItems: []*protocol.DynamicToolCallOutputContentItem{&item},
	}
}

// failedToolCall reports a failure to the model as content plus success=false, so
// the model can read why it failed and try something else.
func failedToolCall(reason string) *protocol.DynamicToolCallResponse {
	item := protocol.NewDynamicToolCallOutputContentItemInputText(
		protocol.DynamicToolCallOutputContentItemInputTextPayload{Text: reason})
	return &protocol.DynamicToolCallResponse{
		Success:      false,
		ContentItems: []*protocol.DynamicToolCallOutputContentItem{&item},
	}
}
