package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Experimental records which definitions, methods, and individual fields exist
// only when app-server runs with --experimental.
//
// The schema carries no machine-readable marker for this. There is no
// @experimental JSDoc tag, no x-* schema keyword, and no naming convention, and
// the prose hints are actively misleading: of 115 experimental-only definitions
// only 15 mention "EXPERIMENTAL", while 41 stable files do, and Thread.path is
// marked [UNSTABLE] yet ships in the stable schema. So the only sound way to
// classify a member is to diff the two schema outputs, which is what this does.
//
// The diff is computed at property-path granularity, not type granularity,
// because eleven support types (ThreadExtra, MultiAgentMode, TurnsPage, and
// others) already exist in the stable schema as orphans with no inbound
// references: only the field that references them is gated. Classifying by type
// presence would mark those fields as stable.
type Experimental struct {
	Defs    map[string]bool // definition name -> exists only with --experimental
	Fields  map[string]bool // "TypeName.fieldName" -> ditto
	Methods map[string]bool // wire method name -> ditto
}

// IsDef reports whether a whole definition is experimental.
func (e *Experimental) IsDef(name string) bool { return e != nil && e.Defs[name] }

// IsField reports whether one field of an otherwise stable type is experimental.
func (e *Experimental) IsField(typeName, field string) bool {
	return e != nil && e.Fields[typeName+"."+field]
}

// IsMethod reports whether a method requires the experimentalApi capability.
func (e *Experimental) IsMethod(method string) bool { return e != nil && e.Methods[method] }

// loadExperimental diffs the vendored experimental schema against the vendored
// stable one. Both are committed; see docs/adr/0004-experimental-schema-diff.md.
func loadExperimental(dir string) (*Experimental, error) {
	stable, err := loadDefinitionsFrom(dir, "stable-")
	if err != nil {
		return nil, fmt.Errorf("stable schema: %w", err)
	}
	experimental, err := loadDefinitionsFrom(dir, "")
	if err != nil {
		return nil, fmt.Errorf("experimental schema: %w", err)
	}

	e := &Experimental{
		Defs:    map[string]bool{},
		Fields:  map[string]bool{},
		Methods: map[string]bool{},
	}

	for name, expDef := range experimental {
		stableDef, inStable := stable[name]
		if !inStable {
			e.Defs[name] = true
			continue
		}
		// The type exists in both: find fields present only in experimental.
		for field := range expDef.Properties {
			if stableDef.Properties == nil {
				e.Fields[name+"."+field] = true
				continue
			}
			if _, ok := stableDef.Properties[field]; !ok {
				e.Fields[name+"."+field] = true
			}
		}
	}

	// Verify the additive-only property the generator relies on, rather than
	// trusting it. It held for codex-cli 0.149.0 but is not documented as a
	// guarantee, so a future version that removes or reshapes something must
	// fail the build instead of silently generating wrong types.
	var regressions []string
	for name, stableDef := range stable {
		expDef, ok := experimental[name]
		if !ok {
			regressions = append(regressions,
				fmt.Sprintf("definition %q exists in the stable schema but not the experimental one", name))
			continue
		}
		for field, stableProp := range stableDef.Properties {
			expProp, ok := expDef.Properties[field]
			if !ok {
				regressions = append(regressions,
					fmt.Sprintf("%s.%s exists in the stable schema but not the experimental one", name, field))
				continue
			}
			a, _ := json.Marshal(stableProp)
			b, _ := json.Marshal(expProp)
			if string(a) != string(b) {
				regressions = append(regressions,
					fmt.Sprintf("%s.%s has a different shape in the experimental schema", name, field))
			}
		}
	}
	if len(regressions) > 0 {
		sort.Strings(regressions)
		if len(regressions) > 12 {
			regressions = append(regressions[:12],
				fmt.Sprintf("... and %d more", len(regressions)-12))
		}
		return nil, fmt.Errorf(
			"the experimental schema is no longer a strict superset of the stable one.\n"+
				"The generator assumes experimental only ADDS optional members; that no longer holds,\n"+
				"so the generated types would be wrong. Re-examine before regenerating:\n  - %s",
			joinLines(regressions))
	}

	methodFiles := []string{
		"ClientRequest.json", "ServerRequest.json",
		"ServerNotification.json", "ClientNotification.json",
	}
	for _, f := range methodFiles {
		stableMethods, err := methodNames(filepath.Join(dir, "stable-"+f))
		if err != nil {
			return nil, err
		}
		expMethods, err := methodNames(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		for m := range expMethods {
			if !stableMethods[m] {
				e.Methods[m] = true
			}
		}
	}
	return e, nil
}

// loadDefinitionsFrom merges both bundles for one variant, identified by a
// filename prefix ("" for experimental, "stable-" for stable).
func loadDefinitionsFrom(dir, prefix string) (map[string]*Schema, error) {
	out := map[string]*Schema{}
	for _, f := range []string{
		"codex_app_server_protocol.schemas.json",
		"codex_app_server_protocol.v2.schemas.json",
	} {
		b, err := os.ReadFile(filepath.Join(dir, prefix+f))
		if err != nil {
			return nil, err
		}
		var bundle Schema
		if err := json.Unmarshal(b, &bundle); err != nil {
			return nil, fmt.Errorf("%s%s: %w", prefix, f, err)
		}
		for n, s := range bundle.Definitions {
			out[n] = s
		}
	}
	return out, nil
}

func methodNames(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root Schema
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]bool{}
	for _, v := range root.OneOf {
		m, ok := v.Properties["method"]
		if !ok || len(m.Enum) != 1 {
			continue
		}
		if name, ok := m.Enum[0].(string); ok {
			out[name] = true
		}
	}
	return out, nil
}

func joinLines(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "\n  - "
		}
		out += s
	}
	return out
}

// experimentalDoc is the annotation placed on experimental members. It names the
// capability rather than the CLI flag, because that is what a Go caller sets.
const experimentalDoc = "EXPERIMENTAL: requires WithExperimentalAPI(). The server rejects this\n" +
	"unless capabilities.experimentalApi was set during initialize."
