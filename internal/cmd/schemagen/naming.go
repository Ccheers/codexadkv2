package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// initialisms are rendered in a canonical case so identifiers read naturally
// and satisfy Go style checkers.
var initialisms = map[string]string{
	"id": "ID", "ids": "IDs", "url": "URL", "urls": "URLs",
	"uri": "URI", "uris": "URIs", "api": "API", "cwd": "Cwd",
	"json": "JSON", "http": "HTTP", "https": "HTTPS", "os": "OS",
	"sha": "SHA", "cli": "CLI", "tui": "TUI", "mcp": "MCP",
	"ttl": "TTL", "pid": "PID", "pty": "PTY", "sdp": "SDP",
	"ui": "UI", "db": "DB", "kb": "KB", "ms": "Ms", "sec": "Sec",
	"cpu": "CPU", "rss": "RSS", "oauth": "OAuth", "sse": "SSE",
	"npm": "NPM", "git": "Git", "toml": "TOML", "md": "MD",
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// exportedIdent converts one wire word into an exported Go identifier fragment,
// applying the initialism table. It never lowercases an already-mixed word, so
// "agentMessage" survives as "AgentMessage" rather than becoming "Agentmessage".
func exportedIdent(word string) string {
	if word == "" {
		return ""
	}
	if canon, ok := initialisms[strings.ToLower(word)]; ok {
		return canon
	}
	r := []rune(word)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// splitWords breaks a wire name into words on non-alphanumeric separators and
// on lower-to-upper camelCase boundaries, so all four casing conventions in the
// schema converge on the same Go identifier:
//
//	"on-request"       -> [on request]
//	"final_answer"     -> [final answer]
//	"AGENTS_MD"        -> [AGENTS MD]
//	"agentMessage"     -> [agent Message]
//	"workspace-write"  -> [workspace write]
func splitWords(s string) []string {
	var words []string
	for _, chunk := range nonAlnum.Split(s, -1) {
		if chunk == "" {
			continue
		}
		// An all-caps chunk is one word: "AGENTS", "MD".
		if chunk == strings.ToUpper(chunk) && len(chunk) > 1 {
			words = append(words, chunk)
			continue
		}
		start := 0
		r := []rune(chunk)
		for i := 1; i < len(r); i++ {
			if unicode.IsUpper(r[i]) && !unicode.IsUpper(r[i-1]) {
				words = append(words, string(r[start:i]))
				start = i
			}
		}
		words = append(words, string(r[start:]))
	}
	return words
}

// goIdent converts any wire name into an exported Go identifier.
func goIdent(s string) string {
	var b strings.Builder
	for _, w := range splitWords(s) {
		b.WriteString(exportedIdent(strings.ToLower(w)))
	}
	out := b.String()
	if out == "" {
		return "Empty"
	}
	// A leading digit is not a legal identifier start.
	if unicode.IsDigit([]rune(out)[0]) {
		return "N" + out
	}
	return out
}

// enumConstName builds the constant name for one enum value. The wire value is
// never derived from this; it is copied verbatim into the constant's value,
// because the schema mixes kebab-case, snake_case, SCREAMING_SNAKE, and
// camelCase across 32 enum types and no convention reproduces them all.
func enumConstName(typeName, value string) string {
	return typeName + goIdent(value)
}

// variantTypeName builds the payload struct name for one union variant:
// SandboxPolicy + "workspaceWrite" -> SandboxPolicyWorkspaceWritePayload.
//
// The "Payload" suffix is required: the discriminant constant for the same
// variant is named SandboxPolicyWorkspaceWrite, and without the suffix the
// struct and the constant collide.
func variantTypeName(unionName, tag string) string {
	return unionName + goIdent(tag) + "Payload"
}

// numericGoType maps a JSON Schema numeric format onto a Go type. The schema
// uses seven distinct formats; int64 and uint64 must not collapse to float64,
// which would silently lose precision on token counts and timestamps.
func numericGoType(format string, isInteger bool) string {
	switch format {
	case "int64":
		return "int64"
	case "int32":
		return "int32"
	case "uint64":
		return "uint64"
	case "uint32":
		return "uint32"
	case "uint16":
		return "uint16"
	case "uint":
		return "uint64"
	case "double", "float":
		return "float64"
	}
	if isInteger {
		return "int64"
	}
	return "float64"
}

// fieldSpec describes how one struct field is rendered.
type fieldSpec struct {
	GoName       string
	WireName     string
	GoType       string
	Doc          string
	Required     bool
	Nullable     bool
	TriState     bool
	Experimental bool
}

// tag renders the struct tag. The rules follow from the schema's four
// combinations of required and nullable:
//
//   - required, non-nullable: plain value, always emitted.
//   - required, nullable:     pointer WITHOUT omitempty, so an explicit null is
//     transmitted rather than dropped. This is the most
//     common shape in the protocol.
//   - optional:               pointer WITH omitempty, so an unset field is absent.
//   - tri-state:              *Nullable[T] with omitempty, distinguishing absent
//     from explicit null.
func (f fieldSpec) tag() string {
	if f.Required && !f.Nullable {
		return fmt.Sprintf("`json:%q`", f.WireName)
	}
	if f.Required && f.Nullable && !f.TriState {
		// Present but possibly null: must serialize as null, so no omitempty.
		return fmt.Sprintf("`json:%q`", f.WireName)
	}
	return fmt.Sprintf("`json:\"%s,omitempty\"`", f.WireName)
}

// goType resolves a property schema to a Go type expression. needsPointer
// reports whether the caller should wrap it so that absence is representable.
func (g *generator) goType(s *Schema, owner, field string) (goType string, needsPointer bool) {
	if s == nil {
		return "json.RawMessage", false
	}

	// A property with no type information at all: four of these exist, all
	// genuinely free-form (outputSchema, MCP content, guardian event).
	if len(s.Type) == 0 && s.Ref == "" && len(s.AllOf) == 0 &&
		len(s.OneOf) == 0 && len(s.AnyOf) == 0 && s.Items == nil && len(s.Properties) == 0 {
		return "json.RawMessage", false
	}

	// A $ref, possibly wrapped in allOf (for a description) or anyOf (for null).
	if name, ok := s.deref(); ok {
		def, known := g.defs[name]
		if !known || def.Kind == KindRaw {
			return "json.RawMessage", false
		}
		switch def.Kind {
		case KindEnum, KindAliasString, KindAliasNumber:
			// Named scalar: nullable only if the schema says so.
			return goIdent(name), s.nullable()
		default:
			return goIdent(name), true
		}
	}

	types := s.types()
	nonNull := make([]string, 0, len(types))
	for _, t := range types {
		if t != "null" {
			nonNull = append(nonNull, t)
		}
	}
	if len(nonNull) != 1 {
		// A union of several concrete JSON types with no discriminant.
		return "json.RawMessage", false
	}

	switch nonNull[0] {
	case "string":
		return "string", s.nullable()
	case "boolean":
		return "bool", s.nullable()
	case "integer":
		return numericGoType(s.Format, true), s.nullable()
	case "number":
		return numericGoType(s.Format, false), s.nullable()
	case "array":
		elem, elemPtr := g.goType(s.Items, owner, field)
		if elemPtr {
			elem = "*" + elem
		}
		// A nil slice already encodes absence; never wrap a slice in a pointer.
		return "[]" + elem, false
	case "object":
		if len(s.Properties) > 0 {
			// An inline nested object. All 12 of these live inside
			// externally-tagged unions and are emitted as named payload structs.
			return g.inlineStructName(owner, field), true
		}
		if len(s.AdditionalProperties) > 0 {
			var sub Schema
			if err := jsonUnmarshalLenient(s.AdditionalProperties, &sub); err == nil && len(sub.Type) > 0 {
				val, valPtr := g.goType(&sub, owner, field)
				if valPtr {
					val = "*" + val
				}
				return "map[string]" + val, false
			}
		}
		return "map[string]json.RawMessage", false
	}
	return "json.RawMessage", false
}

func (g *generator) inlineStructName(owner, field string) string {
	return goIdent(owner) + goIdent(field)
}
