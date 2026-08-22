package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// emitUnions writes the two union families the protocol uses.
//
// Internally tagged (33 types, e.g. SandboxPolicy, ThreadItem, UserInput):
//
//	{"type":"workspaceWrite","writableRoots":[...],"networkAccess":false}
//
// Externally tagged (20 types, e.g. CodexErrorInfo, AskForApproval): a value is
// EITHER a bare string OR a single-key object wrapping the payload:
//
//	"contextWindowExceeded"
//	{"httpConnectionFailed":{"httpStatusCode":503}}
//
// Both render as one Go struct carrying a discriminant plus one pointer per
// variant, with As<Variant>() accessors. Marshaling is driven strictly by the
// discriminant, so a struct whose tag and payload disagree cannot put a
// malformed message on the wire.
func (g *generator) emitUnions(b *bytes.Buffer, names []string) error {
	b.WriteString("import (\n\t\"encoding/json\"\n\t\"fmt\"\n)\n\n")
	b.WriteString(strings.TrimSpace(unionHelpers))
	b.WriteString("\n\n")
	g.emitNullable(b)

	for _, name := range names {
		d := g.defs[name]
		switch d.Kind {
		case KindIntTagged:
			g.emitInternallyTagged(b, name, d)
		case KindExtTagged:
			g.emitExternallyTagged(b, name, d)
		}
	}
	return nil
}

// emitNullable writes the tri-state wrapper. Five fields in the protocol are
// Rust Option<Option<T>>, where absent and explicit-null mean different things:
// absent leaves the server's value unchanged, null clears it. A plain pointer
// cannot express both.
func (g *generator) emitNullable(b *bytes.Buffer) {
	b.WriteString(`// Nullable distinguishes three states for a field where the protocol treats
// "absent" and "explicit null" differently: absent leaves the server's current
// value unchanged, while null clears it.
//
// Use Value to set a value and Null to clear one. A nil *Nullable field is
// omitted from the request entirely.
type Nullable[T any] struct {
	// Valid reports whether Value is meaningful. When false, the field
	// serializes as JSON null.
	Valid bool
	Value T
}

// Value returns a Nullable carrying v.
func Value[T any](v T) *Nullable[T] { return &Nullable[T]{Valid: true, Value: v} }

// Null returns a Nullable that serializes as JSON null, clearing the field.
func Null[T any]() *Nullable[T] { return &Nullable[T]{} }

// MarshalJSON implements json.Marshaler.
func (n Nullable[T]) MarshalJSON() ([]byte, error) {
	if !n.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

// UnmarshalJSON implements json.Unmarshaler.
func (n *Nullable[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		n.Valid = false
		var zero T
		n.Value = zero
		return nil
	}
	n.Valid = true
	return json.Unmarshal(data, &n.Value)
}

`)
}

type unionVariant struct {
	Tag     string  // wire discriminant value
	GoName  string  // accessor/field name fragment
	Payload string  // Go type of the payload, "" when the variant has no fields
	Schema  *Schema // variant schema
	Inline  bool    // payload is an inline object needing its own struct
	Doc     string
}

// emitInternallyTagged writes a union discriminated by a tag field inside the
// object. Variants with no fields beyond the tag carry no payload.
func (g *generator) emitInternallyTagged(b *bytes.Buffer, name string, d Def) {
	goName := goIdent(name)
	variants := g.internalVariants(name, d)

	writeDoc(b, d.Schema.Description)
	fmt.Fprintf(b, "// %s is a tagged union discriminated by its %q field.\n", goName, d.Tag)
	fmt.Fprintf(b, "// Exactly one payload field is set, matching Type.\n")
	fmt.Fprintf(b, "//\n")
	fmt.Fprintf(b, "// A Type this build does not recognize is preserved rather than rejected:\n")
	fmt.Fprintf(b, "// every As method returns false, and Raw holds the original JSON.\n")
	fmt.Fprintf(b, "type %s struct {\n", goName)
	fmt.Fprintf(b, "\tType %sType `json:%q`\n", goName, d.Tag)
	for _, v := range variants {
		if v.Payload == "" {
			continue
		}
		fmt.Fprintf(b, "\t%s *%s `json:\"-\"`\n", v.GoName, v.Payload)
	}
	b.WriteString("\n\t// Raw holds the JSON this value decoded from, so an unrecognized\n")
	b.WriteString("\t// variant or a field added by a newer server is never lost.\n")
	b.WriteString("\tRaw json.RawMessage `json:\"-\"`\n")
	b.WriteString("}\n\n")

	// The discriminant is its own string type with constants, so unknown values
	// survive round-tripping.
	fmt.Fprintf(b, "// %sType is the discriminant of %s.\n", goName, goName)
	fmt.Fprintf(b, "type %sType string\n\n", goName)
	fmt.Fprintf(b, "const (\n")
	for _, v := range variants {
		fmt.Fprintf(b, "\t%s%s %sType = %q\n", goName, v.GoName, goName, v.Tag)
	}
	fmt.Fprintf(b, ")\n\n")

	// Payload structs for variants that carry fields.
	for _, v := range variants {
		if v.Payload == "" || !v.Inline {
			continue
		}
		writeDoc(b, v.Doc)
		g.emitStruct(b, v.Payload, v.Payload, Def{Name: v.Payload, Schema: v.Schema, Kind: KindStruct})
	}

	// Accessors.
	for _, v := range variants {
		if v.Payload == "" {
			fmt.Fprintf(b, "// Is%s reports whether this is the %q variant.\n", v.GoName, v.Tag)
			fmt.Fprintf(b, "func (u *%s) Is%s() bool {\n", goName, v.GoName)
			fmt.Fprintf(b, "\treturn u != nil && u.Type == %s%s\n}\n\n", goName, v.GoName)
			continue
		}
		fmt.Fprintf(b, "// As%s returns the %q payload and true when Type is %q.\n", v.GoName, v.Tag, v.Tag)
		fmt.Fprintf(b, "// It is safe to call on a nil receiver.\n")
		fmt.Fprintf(b, "func (u *%s) As%s() (*%s, bool) {\n", goName, v.GoName, v.Payload)
		fmt.Fprintf(b, "\tif u == nil || u.Type != %s%s {\n\t\treturn nil, false\n\t}\n", goName, v.GoName)
		fmt.Fprintf(b, "\treturn u.%s, u.%s != nil\n}\n\n", v.GoName, v.GoName)
	}

	// Constructors set tag and payload together, so an inconsistent value cannot
	// be built by accident.
	for _, v := range variants {
		if v.Payload == "" {
			fmt.Fprintf(b, "// New%s%s returns the %q variant.\n", goName, v.GoName, v.Tag)
			fmt.Fprintf(b, "func New%s%s() %s {\n", goName, v.GoName, goName)
			fmt.Fprintf(b, "\treturn %s{Type: %s%s}\n}\n\n", goName, goName, v.GoName)
			continue
		}
		fmt.Fprintf(b, "// New%s%s returns the %q variant carrying p.\n", goName, v.GoName, v.Tag)
		fmt.Fprintf(b, "func New%s%s(p %s) %s {\n", goName, v.GoName, v.Payload, goName)
		fmt.Fprintf(b, "\treturn %s{Type: %s%s, %s: &p}\n}\n\n", goName, goName, v.GoName, v.GoName)
	}

	g.emitInternalCodec(b, goName, d.Tag, variants)
}

// emitInternalCodec writes MarshalJSON/UnmarshalJSON for an internally tagged
// union. Decoding reads the tag, then decodes the whole object into the matching
// payload, because the payload's fields sit alongside the tag rather than nested.
func (g *generator) emitInternalCodec(b *bytes.Buffer, goName, tag string, variants []unionVariant) {
	fmt.Fprintf(b, "// UnmarshalJSON implements json.Unmarshaler. An unrecognized %q is\n", tag)
	fmt.Fprintf(b, "// retained in Type and Raw rather than treated as an error.\n")
	fmt.Fprintf(b, "func (u *%s) UnmarshalJSON(data []byte) error {\n", goName)
	fmt.Fprintf(b, "\tvar probe struct {\n\t\tType %sType `json:%q`\n\t}\n", goName, tag)
	b.WriteString("\tif err := json.Unmarshal(data, &probe); err != nil {\n")
	fmt.Fprintf(b, "\t\treturn fmt.Errorf(\"%s: %%w\", err)\n\t}\n", goName)
	b.WriteString("\tu.Type = probe.Type\n")
	b.WriteString("\tu.Raw = append(u.Raw[:0], data...)\n")
	b.WriteString("\tswitch probe.Type {\n")
	for _, v := range variants {
		if v.Payload == "" {
			continue
		}
		fmt.Fprintf(b, "\tcase %s%s:\n", goName, v.GoName)
		fmt.Fprintf(b, "\t\tvar p %s\n", v.Payload)
		b.WriteString("\t\tif err := json.Unmarshal(data, &p); err != nil {\n")
		fmt.Fprintf(b, "\t\t\treturn fmt.Errorf(\"%s %%s: %%w\", probe.Type, err)\n\t\t}\n", goName)
		fmt.Fprintf(b, "\t\tu.%s = &p\n", v.GoName)
	}
	b.WriteString("\t}\n\treturn nil\n}\n\n")

	fmt.Fprintf(b, "// MarshalJSON implements json.Marshaler. Output is driven strictly by Type,\n")
	fmt.Fprintf(b, "// so a payload field that does not match Type is ignored rather than\n")
	fmt.Fprintf(b, "// producing a message the server would reject.\n")
	fmt.Fprintf(b, "func (u %s) MarshalJSON() ([]byte, error) {\n", goName)
	b.WriteString("\tif u.Type == \"\" {\n")
	if len(variants) > 0 {
		fmt.Fprintf(b, "\t\tif len(u.Raw) > 0 {\n\t\t\treturn u.Raw, nil\n\t\t}\n")
	}
	fmt.Fprintf(b, "\t\treturn nil, fmt.Errorf(\"%s: Type is empty\")\n\t}\n", goName)
	b.WriteString("\tswitch u.Type {\n")
	for _, v := range variants {
		fmt.Fprintf(b, "\tcase %s%s:\n", goName, v.GoName)
		if v.Payload == "" {
			fmt.Fprintf(b, "\t\treturn json.Marshal(map[string]any{%q: u.Type})\n", tag)
			continue
		}
		fmt.Fprintf(b, "\t\tif u.%s == nil {\n", v.GoName)
		fmt.Fprintf(b, "\t\t\treturn nil, fmt.Errorf(\"%s: Type is %%q but %s is nil\", u.Type)\n", goName, v.GoName)
		b.WriteString("\t\t}\n")
		// Merge the tag into the payload's own object.
		fmt.Fprintf(b, "\t\treturn marshalTagged(%q, string(u.Type), u.%s)\n", tag, v.GoName)
	}
	b.WriteString("\t}\n")
	b.WriteString("\t// An unrecognized variant round-trips through Raw so nothing is lost.\n")
	b.WriteString("\tif len(u.Raw) > 0 {\n\t\treturn u.Raw, nil\n\t}\n")
	fmt.Fprintf(b, "\treturn json.Marshal(map[string]any{%q: u.Type})\n}\n\n", tag)
}

func (g *generator) internalVariants(name string, d Def) []unionVariant {
	goName := goIdent(name)
	schemas := d.Schema.OneOf
	if len(schemas) == 0 {
		schemas = d.Schema.AnyOf
	}
	var out []unionVariant
	for _, v := range schemas {
		if v.hasType("null") {
			continue
		}
		p, ok := v.Properties[d.Tag]
		if !ok || len(p.Enum) != 1 {
			continue
		}
		tag, _ := p.Enum[0].(string)
		uv := unionVariant{
			Tag:    tag,
			GoName: goIdent(tag),
			Schema: v,
			Doc:    v.Description,
		}
		// Does the variant carry anything beyond the tag?
		extra := false
		for k := range v.Properties {
			if k != d.Tag {
				extra = true
				break
			}
		}
		if extra {
			uv.Payload = variantTypeName(goName, tag)
			uv.Inline = true
			// The payload struct must not re-declare the tag field.
			uv.Schema = withoutProperty(v, d.Tag)
		}
		out = append(out, uv)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// emitExternallyTagged writes a union whose value is either a bare string or a
// single-key object. This is serde's default representation for a Rust enum
// where some variants carry data and others do not.
func (g *generator) emitExternallyTagged(b *bytes.Buffer, name string, d Def) {
	goName := goIdent(name)
	bare, payloads := g.externalVariants(name, d)

	writeDoc(b, d.Schema.Description)
	fmt.Fprintf(b, "// %s is an externally tagged union. On the wire it is either a bare\n", goName)
	fmt.Fprintf(b, "// string naming the variant, or a single-key object wrapping a payload.\n")
	fmt.Fprintf(b, "//\n")
	fmt.Fprintf(b, "// A variant this build does not recognize is preserved rather than\n")
	fmt.Fprintf(b, "// rejected: Kind holds the name and Raw holds the original JSON.\n")
	fmt.Fprintf(b, "type %s struct {\n", goName)
	fmt.Fprintf(b, "\tKind %sKind `json:\"-\"`\n", goName)
	for _, v := range payloads {
		fmt.Fprintf(b, "\t%s *%s `json:\"-\"`\n", v.GoName, v.Payload)
	}
	b.WriteString("\n\t// Raw holds the JSON this value decoded from.\n")
	b.WriteString("\tRaw json.RawMessage `json:\"-\"`\n")
	b.WriteString("}\n\n")

	fmt.Fprintf(b, "// %sKind names the variant of %s.\n", goName, goName)
	fmt.Fprintf(b, "type %sKind string\n\n", goName)
	fmt.Fprintf(b, "const (\n")
	for _, v := range append(append([]unionVariant{}, bare...), payloads...) {
		fmt.Fprintf(b, "\t%s%s %sKind = %q\n", goName, v.GoName, goName, v.Tag)
	}
	fmt.Fprintf(b, ")\n\n")

	for _, v := range payloads {
		if !v.Inline {
			continue
		}
		writeDoc(b, v.Doc)
		g.emitStruct(b, v.Payload, v.Payload, Def{Name: v.Payload, Schema: v.Schema, Kind: KindStruct})
	}

	for _, v := range bare {
		fmt.Fprintf(b, "// Is%s reports whether this is the %q variant.\n", v.GoName, v.Tag)
		fmt.Fprintf(b, "func (u *%s) Is%s() bool {\n", goName, v.GoName)
		fmt.Fprintf(b, "\treturn u != nil && u.Kind == %s%s\n}\n\n", goName, v.GoName)
	}
	for _, v := range payloads {
		fmt.Fprintf(b, "// As%s returns the %q payload and true when Kind is %q.\n", v.GoName, v.Tag, v.Tag)
		fmt.Fprintf(b, "// It is safe to call on a nil receiver.\n")
		fmt.Fprintf(b, "func (u *%s) As%s() (*%s, bool) {\n", goName, v.GoName, v.Payload)
		fmt.Fprintf(b, "\tif u == nil || u.Kind != %s%s {\n\t\treturn nil, false\n\t}\n", goName, v.GoName)
		fmt.Fprintf(b, "\treturn u.%s, u.%s != nil\n}\n\n", v.GoName, v.GoName)
	}

	for _, v := range bare {
		fmt.Fprintf(b, "// New%s%s returns the %q variant.\n", goName, v.GoName, v.Tag)
		fmt.Fprintf(b, "func New%s%s() %s {\n", goName, v.GoName, goName)
		fmt.Fprintf(b, "\treturn %s{Kind: %s%s}\n}\n\n", goName, goName, v.GoName)
	}
	for _, v := range payloads {
		fmt.Fprintf(b, "// New%s%s returns the %q variant carrying p.\n", goName, v.GoName, v.Tag)
		fmt.Fprintf(b, "func New%s%s(p %s) %s {\n", goName, v.GoName, v.Payload, goName)
		fmt.Fprintf(b, "\treturn %s{Kind: %s%s, %s: &p}\n}\n\n", goName, goName, v.GoName, v.GoName)
	}

	// Codec: probe for a string first, then for a single-key object.
	fmt.Fprintf(b, "// UnmarshalJSON implements json.Unmarshaler.\n")
	fmt.Fprintf(b, "func (u *%s) UnmarshalJSON(data []byte) error {\n", goName)
	b.WriteString("\tu.Raw = append(u.Raw[:0], data...)\n")
	b.WriteString("\tvar bare string\n")
	b.WriteString("\tif err := json.Unmarshal(data, &bare); err == nil {\n")
	fmt.Fprintf(b, "\t\tu.Kind = %sKind(bare)\n\t\treturn nil\n\t}\n", goName)
	b.WriteString("\tvar obj map[string]json.RawMessage\n")
	b.WriteString("\tif err := json.Unmarshal(data, &obj); err != nil {\n")
	fmt.Fprintf(b, "\t\treturn fmt.Errorf(\"%s: expected a string or an object: %%w\", err)\n\t}\n", goName)
	b.WriteString("\tif len(obj) != 1 {\n")
	fmt.Fprintf(b, "\t\treturn fmt.Errorf(\"%s: expected exactly one key, got %%d\", len(obj))\n\t}\n", goName)
	if len(payloads) == 0 {
		// Every arm of this union is a bare string, so the object form carries
		// nothing to decode. Record the key as the variant name and stop.
		b.WriteString("\tfor key := range obj {\n")
		fmt.Fprintf(b, "\t\tu.Kind = %sKind(key)\n\t}\n\treturn nil\n}\n\n", goName)
	} else {
		b.WriteString("\tfor key, payload := range obj {\n")
		fmt.Fprintf(b, "\t\tu.Kind = %sKind(key)\n", goName)
		b.WriteString("\t\tswitch key {\n")
		for _, v := range payloads {
			fmt.Fprintf(b, "\t\tcase %q:\n", v.Tag)
			fmt.Fprintf(b, "\t\t\tvar p %s\n", v.Payload)
			b.WriteString("\t\t\tif err := json.Unmarshal(payload, &p); err != nil {\n")
			fmt.Fprintf(b, "\t\t\t\treturn fmt.Errorf(\"%s %%s: %%w\", key, err)\n\t\t\t}\n", goName)
			fmt.Fprintf(b, "\t\t\tu.%s = &p\n", v.GoName)
		}
		b.WriteString("\t\tdefault:\n")
		b.WriteString("\t\t\t// A variant added by a newer server: Kind and Raw preserve it.\n")
		b.WriteString("\t\t\t_ = payload\n")
		b.WriteString("\t\t}\n\t}\n\treturn nil\n}\n\n")
	}

	fmt.Fprintf(b, "// MarshalJSON implements json.Marshaler. Output is driven strictly by Kind.\n")
	fmt.Fprintf(b, "func (u %s) MarshalJSON() ([]byte, error) {\n", goName)
	b.WriteString("\tif u.Kind == \"\" {\n")
	b.WriteString("\t\tif len(u.Raw) > 0 {\n\t\t\treturn u.Raw, nil\n\t\t}\n")
	fmt.Fprintf(b, "\t\treturn nil, fmt.Errorf(\"%s: Kind is empty\")\n\t}\n", goName)
	b.WriteString("\tswitch u.Kind {\n")
	for _, v := range payloads {
		fmt.Fprintf(b, "\tcase %s%s:\n", goName, v.GoName)
		fmt.Fprintf(b, "\t\tif u.%s == nil {\n", v.GoName)
		fmt.Fprintf(b, "\t\t\treturn nil, fmt.Errorf(\"%s: Kind is %%q but %s is nil\", u.Kind)\n", goName, v.GoName)
		b.WriteString("\t\t}\n")
		fmt.Fprintf(b, "\t\treturn json.Marshal(map[string]any{%q: u.%s})\n", v.Tag, v.GoName)
	}
	b.WriteString("\t}\n")
	b.WriteString("\t// Bare variants, and any variant this build does not know, serialize as\n")
	b.WriteString("\t// the plain string form.\n")
	b.WriteString("\treturn json.Marshal(string(u.Kind))\n}\n\n")
}

func (g *generator) externalVariants(name string, d Def) (bare, payloads []unionVariant) {
	goName := goIdent(name)
	schemas := d.Schema.OneOf
	if len(schemas) == 0 {
		schemas = d.Schema.AnyOf
	}
	for _, v := range schemas {
		if v.hasType("null") {
			continue
		}
		// Bare string arm: an enum of one or more plain names.
		if v.hasType("string") && len(v.Enum) > 0 {
			for _, raw := range v.Enum {
				s, ok := raw.(string)
				if !ok {
					continue
				}
				bare = append(bare, unionVariant{Tag: s, GoName: goIdent(s)})
			}
			continue
		}
		// Object arm: exactly one key, whose value is the payload.
		if len(v.Properties) != 1 {
			continue
		}
		for key, payload := range v.Properties {
			uv := unionVariant{
				Tag:    key,
				GoName: goIdent(key),
				Doc:    v.Description,
			}
			if payload.hasType("object") && len(payload.Properties) > 0 {
				uv.Payload = variantTypeName(goName, key)
				uv.Schema = payload
				uv.Inline = true
			} else if ref, ok := payload.deref(); ok {
				if def, known := g.defs[ref]; known && def.Kind != KindRaw {
					uv.Payload = goIdent(ref)
				} else {
					uv.Payload = "json.RawMessage"
				}
			} else if payload.hasType("string") {
				uv.Payload = "string"
			} else {
				uv.Payload = "json.RawMessage"
			}
			payloads = append(payloads, uv)
		}
	}
	sort.SliceStable(bare, func(i, j int) bool { return bare[i].Tag < bare[j].Tag })
	sort.SliceStable(payloads, func(i, j int) bool { return payloads[i].Tag < payloads[j].Tag })
	return bare, payloads
}

// withoutProperty returns a copy of s with one property removed, so a variant
// payload struct does not re-declare the discriminant field.
func withoutProperty(s *Schema, drop string) *Schema {
	out := *s
	out.Properties = make(map[string]*Schema, len(s.Properties))
	for k, v := range s.Properties {
		if k != drop {
			out.Properties[k] = v
		}
	}
	out.Required = nil
	for _, r := range s.Required {
		if r != drop {
			out.Required = append(out.Required, r)
		}
	}
	return &out
}

// unionHelpers is hand-written support code copied verbatim into the generated
// package, so the generated files have no dependency outside the stdlib.
const unionHelpers = `
// marshalTagged serializes payload as a JSON object with an extra discriminant
// key merged in at the top level. Internally tagged unions place the tag
// alongside the payload's own fields rather than nesting them.
func marshalTagged(tagKey, tagValue string, payload any) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	tag, err := json.Marshal(tagValue)
	if err != nil {
		return nil, err
	}
	fields[tagKey] = tag
	return json.Marshal(fields)
}
`
