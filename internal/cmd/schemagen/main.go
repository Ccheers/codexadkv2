// Package main implements schemagen, the code generator for codex/protocol.
//
// It reads the JSON Schema bundles vendored in internal/schemas and emits Go
// types into codex/protocol. See docs/adr/0001-generated-protocol-types.md for
// why the pipeline is shaped this way.
//
// Run it with `go generate ./...` or `make generate`. The generated output is
// committed; CI verifies it is current.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Schema is the subset of JSON Schema draft-07 that the Codex bundles use.
type Schema struct {
	Ref                  string             `json:"$ref"`
	Title                string             `json:"title"`
	Description          string             `json:"description"`
	Type                 json.RawMessage    `json:"type"`
	Format               string             `json:"format"`
	Enum                 []any              `json:"enum"`
	Properties           map[string]*Schema `json:"properties"`
	Required             []string           `json:"required"`
	Items                *Schema            `json:"items"`
	OneOf                []*Schema          `json:"oneOf"`
	AnyOf                []*Schema          `json:"anyOf"`
	AllOf                []*Schema          `json:"allOf"`
	AdditionalProperties json.RawMessage    `json:"additionalProperties"`
	Definitions          map[string]*Schema `json:"definitions"`
	Default              json.RawMessage    `json:"default"`

	// AlwaysValid is set when the schema was the bare boolean `true`, meaning
	// any value is accepted. Such properties become json.RawMessage.
	AlwaysValid bool `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler.
//
// JSON Schema permits a bare boolean anywhere a schema is expected: `true`
// accepts any value, `false` accepts none. The bundles use this for
// free-form properties, so decoding must tolerate it rather than fail.
func (s *Schema) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		s.AlwaysValid = b
		return nil
	}
	type plain Schema // avoid recursing into this method
	return json.Unmarshal(data, (*plain)(s))
}

// types returns the declared JSON types. The bundles use both a bare string
// ("type":"string") and a list ("type":["string","null"]) to express
// nullability, so both forms must be handled.
func (s *Schema) types() []string {
	if len(s.Type) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(s.Type, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(s.Type, &many); err == nil {
		return many
	}
	return nil
}

func (s *Schema) hasType(want string) bool {
	for _, t := range s.types() {
		if t == want {
			return true
		}
	}
	return false
}

// nullable reports whether null is an accepted value, in either the
// "type":[...,"null"] form or the anyOf:[{$ref},{"type":"null"}] form.
func (s *Schema) nullable() bool {
	if s.hasType("null") {
		return true
	}
	for _, v := range append(append([]*Schema{}, s.OneOf...), s.AnyOf...) {
		if v.hasType("null") {
			return true
		}
	}
	return false
}

// refName strips the "#/definitions/" prefix from a $ref.
func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// deref resolves a schema that is only a wrapper around a single $ref. The
// bundles wrap refs in allOf (to attach a description) and in anyOf (to add
// null), so a property's real target is often two levels down.
func (s *Schema) deref() (string, bool) {
	if s.Ref != "" {
		return refName(s.Ref), true
	}
	if len(s.AllOf) == 1 && s.AllOf[0].Ref != "" {
		return refName(s.AllOf[0].Ref), true
	}
	// anyOf: [{$ref}, {"type":"null"}]
	var found string
	for _, v := range append(append([]*Schema{}, s.OneOf...), s.AnyOf...) {
		if v.hasType("null") {
			continue
		}
		if v.Ref == "" {
			return "", false
		}
		if found != "" {
			return "", false
		}
		found = refName(v.Ref)
	}
	return found, found != ""
}

// Kind classifies a definition into the code shape the generator must emit.
type Kind int

const (
	KindStruct Kind = iota // an object with properties
	KindEnum               // a bare string enum
	KindAliasString
	KindAliasNumber
	KindIntTagged // {"type":"a",...} | {"type":"b",...} — discriminated on a tag field
	KindExtTagged // "bare" | {"variant":{...}} — serde external tagging
	KindRaw       // anything we deliberately do not model
)

// Def is one classified schema definition.
type Def struct {
	Name   string
	Schema *Schema
	Kind   Kind
	Tag    string // discriminant field name for KindIntTagged: type, kind, or mode
}

// tagFields are the discriminant names used by internally-tagged unions. "type"
// dominates, but "kind" (FileSystemSpecialPath) and "mode"
// (McpServerElicitationRequestParams) also occur.
var tagFields = []string{"type", "kind", "mode"}

// rawTypes are modelled as json.RawMessage rather than generated. The
// McpElicitation* family is a four-level-deep untagged union disambiguated only
// by structural inspection; JsonValue is genuinely recursive; the JSONRPC*
// envelope types are owned by internal/jsonrpc.
var rawTypes = map[string]bool{
	"JsonValue":                            true,
	"JSONRPCMessage":                       true,
	"JSONRPCRequest":                       true,
	"JSONRPCResponse":                      true,
	"JSONRPCNotification":                  true,
	"JSONRPCError":                         true,
	"JSONRPCErrorError":                    true,
	"McpElicitationPrimitiveSchema":        true,
	"McpElicitationEnumSchema":             true,
	"McpElicitationSingleSelectEnumSchema": true,
	"McpElicitationMultiSelectEnumSchema":  true,
	"ResourceContent":                      true,
	"HookMetadata":                         true,
	"RequestId":                            true,
	"ForcedChatgptWorkspaceIds":            true,
	"FunctionCallOutputBody":               true,
	"ThreadListCwdFilter":                  true,
	"NullableGetAccountTokenUsageParams":   true,
}

// skipTypes are the umbrella unions. Dispatch is keyed on method strings
// instead; generating these would drag in 217 types for ServerNotification
// alone and produce an unusable API.
var skipTypes = map[string]bool{
	"ClientRequest":      true,
	"ServerRequest":      true,
	"ServerNotification": true,
	"ClientNotification": true,
}

// triStateFields are fields whose Rust type is Option<Option<T>>, meaning
// absent and explicit-null are *different*: absent leaves the value unchanged,
// null clears it. The JSON Schema collapses this to ["string","null"] and loses
// the distinction, so it must be recorded by hand. Only the TypeScript output
// preserves it (as `| null | null`). Keyed "TypeName.fieldName".
//
// To re-derive this list after a schema upgrade, since the JSON Schema cannot
// tell you:
//
//	codex app-server generate-ts --out /tmp/codex-ts --experimental
//	grep -rl "null | null" /tmp/codex-ts
//
// Every field matching `| null | null` in that output belongs here. A missing
// entry is a silent bug: the field generates as a plain pointer, and callers
// have no way to express "clear this value".
var triStateFields = map[string]bool{
	"ThreadStartParams.serviceTier":          true,
	"ThreadResumeParams.serviceTier":         true,
	"ThreadForkParams.serviceTier":           true,
	"TurnStartParams.serviceTier":            true,
	"CollaborationModeMask.reasoning_effort": true,

	// These two were missed when the table was first written and found later by
	// re-running the grep above. Both are on types the typed API does not wrap,
	// which is why nothing caught them sooner.
	"ThreadSettingsUpdateParams.serviceTier": true,
	"ThreadRealtimeStartParams.prompt":       true,
}

func classify(name string, s *Schema) Def {
	d := Def{Name: name, Schema: s}
	switch {
	case rawTypes[name]:
		d.Kind = KindRaw
	case s.hasType("string") && len(s.Enum) > 0:
		d.Kind = KindEnum
	case s.hasType("string"):
		d.Kind = KindAliasString
	case s.hasType("integer") || s.hasType("number"):
		d.Kind = KindAliasNumber
	case len(s.OneOf) > 0 || len(s.AnyOf) > 0:
		variants := s.OneOf
		if len(variants) == 0 {
			variants = s.AnyOf
		}
		if tag := findTag(variants); tag != "" {
			d.Kind, d.Tag = KindIntTagged, tag
		} else {
			d.Kind = KindExtTagged
		}
	default:
		d.Kind = KindStruct
	}
	return d
}

// findTag returns the discriminant field name if every object variant carries
// the same single-valued enum field, which is what serde's internal tagging
// produces. It returns "" for externally-tagged and untagged unions.
func findTag(variants []*Schema) string {
	for _, candidate := range tagFields {
		ok, seen := true, 0
		for _, v := range variants {
			if v.hasType("null") {
				continue
			}
			p, has := v.Properties[candidate]
			if !has || len(p.Enum) != 1 {
				ok = false
				break
			}
			seen++
		}
		if ok && seen > 0 {
			return candidate
		}
	}
	return ""
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	schemaDir := filepath.Join(root, "internal", "schemas")

	defs, err := loadDefinitions(schemaDir)
	if err != nil {
		return err
	}
	version, err := os.ReadFile(filepath.Join(schemaDir, "VERSION"))
	if err != nil {
		return err
	}

	exp, err := loadExperimental(schemaDir)
	if err != nil {
		return err
	}

	methods, err := loadMethods(schemaDir)
	if err != nil {
		return err
	}
	if err := methods.validate(defs); err != nil {
		return err
	}

	names := make([]string, 0, len(defs))
	for n := range defs {
		if !skipTypes[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	classified := make(map[string]Def, len(names))
	for _, n := range names {
		classified[n] = classify(n, defs[n])
	}

	g := &generator{
		defs:    classified,
		methods: methods,
		version: strings.TrimSpace(string(version)),
		exp:     exp,
	}
	outDir := filepath.Join(root, "codex", "protocol")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	return g.emitAll(outDir, names)
}

// repoRoot walks up from the working directory looking for go.mod, so the
// generator works from any directory.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above working directory")
		}
		dir = parent
	}
}

// loadDefinitions merges both bundles into one namespace. They are
// complementary: the v1 bundle holds root types such as InitializeResponse, the
// v2 bundle holds all Thread/Turn/Item types. Of the 12 colliding names only
// RequestId and the two umbrella unions differ, and all three are handled
// specially, so a straight merge is safe.
func loadDefinitions(dir string) (map[string]*Schema, error) {
	out := map[string]*Schema{}
	for _, f := range []string{
		"codex_app_server_protocol.schemas.json",
		"codex_app_server_protocol.v2.schemas.json",
	} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		var bundle Schema
		if err := json.Unmarshal(b, &bundle); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for n, s := range bundle.Definitions {
			out[n] = s
		}
	}
	return out, nil
}
