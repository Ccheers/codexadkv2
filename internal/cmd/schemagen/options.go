package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// emitOptions writes a constructor and a functional option per field for every
// generated struct.
//
// The shape is:
//
//	params := protocol.NewTurnStartParams("thr_1", input,
//	    protocol.WithTurnStartParamsModel("gpt-5.6-terra"),
//	    protocol.WithTurnStartParamsCwd("/repo"),
//	)
//
// Required fields become positional constructor arguments so they cannot be
// forgotten; optional fields become options. That split is the point: the schema
// knows which fields are required, and encoding it in the signature moves a class
// of mistake from runtime to compile time.
//
// Options take the *underlying* value rather than a pointer, so callers write
// WithXCwd("/repo") instead of building a temporary variable to take its address.
// The option does the pointer-taking. For tri-state fields the option takes a
// value and a separate Clear option emits an explicit null, since those two mean
// different things on the wire.
//
// Every option name is prefixed with its type name. Go has no per-type namespace
// for functions, and dozens of types share field names like Model, Cwd, and
// ThreadID, so unprefixed names would collide immediately.
func (g *generator) emitOptions(b *bytes.Buffer, _ []string) error {
	b.WriteString("import \"encoding/json\"\n\n")
	b.WriteString(`// This file gives every generated struct a constructor and one functional
// option per optional field.
//
// Required fields are constructor arguments; optional fields are options. Options
// accept plain values and handle pointer-taking internally, so a caller never
// needs a temporary variable just to take an address.
//
// Structs remain plain structs: a literal still works, and these are a
// convenience, not a required construction path.

`)

	// Deduplicate: a struct can be registered more than once when a schema type
	// is reachable by several paths, and Go will not tolerate redeclaration.
	seen := make(map[string]bool, len(g.structs))
	unique := make([]emittedStruct, 0, len(g.structs))
	for _, st := range g.structs {
		if seen[st.GoName] {
			continue
		}
		seen[st.GoName] = true
		unique = append(unique, st)
	}
	sort.SliceStable(unique, func(i, j int) bool { return unique[i].GoName < unique[j].GoName })

	// One option type per struct, so options for different structs cannot be
	// passed to the wrong constructor.
	for _, st := range unique {
		g.emitOptionType(b, st)
	}
	for _, st := range unique {
		g.emitConstructor(b, st)
		g.emitFieldOptions(b, st)
	}
	return nil
}

// optionTypeName is the name of the functional-option type for a struct.
//
// Three schema types already end in "Option" (ReasoningEffortOption,
// ToolRequestUserInputOption, McpElicitationConstOption), so a bare "Option"
// suffix would produce ReasoningEffortOptionOption. "Opt" keeps those readable
// while staying obvious everywhere else.
func optionTypeName(goName string) string {
	if strings.HasSuffix(goName, "Option") {
		return goName + "Opt"
	}
	return goName + "Option"
}

// emitOptionType declares the per-struct option type. Making it distinct per
// struct means the compiler rejects WithThreadStartParamsModel(...) passed to
// NewTurnStartParams, which an untyped func(any) would silently accept.
func (g *generator) emitOptionType(b *bytes.Buffer, st emittedStruct) {
	fmt.Fprintf(b, "// %s configures a %s.\n", optionTypeName(st.GoName), st.GoName)
	fmt.Fprintf(b, "type %s func(*%s)\n\n", optionTypeName(st.GoName), st.GoName)
}

// emitConstructor writes New<Type>(required..., opts...).
func (g *generator) emitConstructor(b *bytes.Buffer, st emittedStruct) {
	required := requiredFields(st.Fields)

	fmt.Fprintf(b, "// New%s returns a %s.\n", st.GoName, st.GoName)
	if len(required) > 0 {
		b.WriteString("//\n")
		b.WriteString("// The parameters are the fields the protocol requires; everything else is\n")
		b.WriteString("// optional and can be set with the With" + st.GoName + "... options.\n")
	}
	if g.exp.IsDef(st.SchemaName) {
		b.WriteString("//\n")
		for _, line := range strings.Split(experimentalDoc, "\n") {
			fmt.Fprintf(b, "// %s\n", line)
		}
	}

	args := make([]string, 0, len(required)+1)
	for _, f := range required {
		args = append(args, fmt.Sprintf("%s %s", argName(f.GoName), f.GoType))
	}
	args = append(args, fmt.Sprintf("opts ...%s", optionTypeName(st.GoName)))

	fmt.Fprintf(b, "func New%s(%s) %s {\n", st.GoName, strings.Join(args, ", "), st.GoName)
	fmt.Fprintf(b, "\tv := %s{\n", st.GoName)
	for _, f := range required {
		fmt.Fprintf(b, "\t\t%s: %s,\n", f.GoName, argName(f.GoName))
	}
	b.WriteString("\t}\n")
	b.WriteString("\tfor _, opt := range opts {\n\t\topt(&v)\n\t}\n")
	b.WriteString("\treturn v\n}\n\n")
}

// emitFieldOptions writes one option per optional field.
func (g *generator) emitFieldOptions(b *bytes.Buffer, st emittedStruct) {
	for _, f := range st.Fields {
		if f.Required && !f.TriState {
			// Required fields are constructor arguments. Emitting an option too
			// would invite setting them twice with no indication which wins.
			continue
		}
		g.emitFieldOption(b, st, f)
	}
}

func (g *generator) emitFieldOption(b *bytes.Buffer, st emittedStruct, f fieldSpec) {
	name := "With" + st.GoName + f.GoName

	// Tri-state fields need two options, because absent, null, and a value are
	// three distinct wire states and one setter cannot express all three.
	if f.TriState {
		inner := strings.TrimSuffix(strings.TrimPrefix(f.GoType, "*Nullable["), "]")
		writeFieldDoc(b, f)
		fmt.Fprintf(b, "// %s sets %s.\n", name, f.WireName)
		fmt.Fprintf(b, "func %s(v %s) %s {\n", name, inner, optionTypeName(st.GoName))
		fmt.Fprintf(b, "\treturn func(o *%s) { o.%s = Value(v) }\n}\n\n", st.GoName, f.GoName)

		clear := "Clear" + st.GoName + f.GoName
		fmt.Fprintf(b, "// %s sends an explicit null for %s, which clears the value\n", clear, f.WireName)
		fmt.Fprintf(b, "// server-side. Leaving the field unset instead leaves it unchanged.\n")
		fmt.Fprintf(b, "func %s() %s {\n", clear, optionTypeName(st.GoName))
		fmt.Fprintf(b, "\treturn func(o *%s) { o.%s = Null[%s]() }\n}\n\n", st.GoName, f.GoName, inner)
		return
	}

	writeFieldDoc(b, f)
	fmt.Fprintf(b, "// %s sets %s.\n", name, f.WireName)
	if f.Experimental {
		for _, line := range strings.Split(experimentalDoc, "\n") {
			fmt.Fprintf(b, "// %s\n", line)
		}
	}

	// A pointer field takes the underlying value and has the option take the
	// address, which is the whole ergonomic win. Slices, maps and RawMessage are
	// already nilable and pass through unchanged.
	if strings.HasPrefix(f.GoType, "*") {
		inner := strings.TrimPrefix(f.GoType, "*")
		fmt.Fprintf(b, "func %s(v %s) %s {\n", name, inner, optionTypeName(st.GoName))
		fmt.Fprintf(b, "\treturn func(o *%s) { o.%s = &v }\n}\n\n", st.GoName, f.GoName)
		return
	}

	fmt.Fprintf(b, "func %s(v %s) %s {\n", name, f.GoType, optionTypeName(st.GoName))
	fmt.Fprintf(b, "\treturn func(o *%s) { o.%s = v }\n}\n\n", st.GoName, f.GoName)
}

// writeFieldDoc copies the schema's own description onto the option, so godoc for
// the option says what the field means rather than only naming it.
func writeFieldDoc(b *bytes.Buffer, f fieldSpec) {
	if f.Doc == "" {
		return
	}
	for _, line := range strings.Split(f.Doc, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			fmt.Fprintf(b, "// %s\n", line)
		}
	}
}

// requiredFields returns the fields that become positional constructor
// arguments, in a stable order.
//
// Tri-state fields are excluded even when required: their whole point is
// distinguishing absent from null, and a positional argument cannot express
// "absent". They get options instead.
func requiredFields(fields []fieldSpec) []fieldSpec {
	var out []fieldSpec
	for _, f := range fields {
		if f.Required && !f.TriState {
			out = append(out, f)
		}
	}
	return out
}

// argName converts a field name into a parameter name that will not collide with
// a package-level identifier or a Go keyword.
func argName(goName string) string {
	if goName == "" {
		return "v"
	}
	r := []rune(goName)
	// Leading initialisms are fully upper-case ("IDs", "URL"); lower the whole
	// run so the parameter reads as one word.
	i := 0
	for i < len(r) && r[i] >= 'A' && r[i] <= 'Z' {
		i++
	}
	if i > 1 && i < len(r) {
		i-- // keep the last upper-case rune as the start of the next word
	}
	for j := 0; j < i; j++ {
		r[j] = r[j] + ('a' - 'A')
	}
	name := string(r)
	if goKeywords[name] {
		return name + "_"
	}
	return name
}

// goKeywords are reserved words that cannot be used as parameter names.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
	// Not keywords, but shadowing these inside a constructor is confusing.
	"len": true, "cap": true, "new": true, "make": true, "copy": true, "string": true,
}
