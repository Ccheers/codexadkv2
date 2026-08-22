package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MethodEntry is one method-to-payload mapping read from the wire-union files.
type MethodEntry struct {
	Method string // wire method name, e.g. "thread/start"
	Params string // params definition name, "" when the method takes no params
	Result string // result definition name, "" when the result is empty
	GoName string // Go identifier derived from Method

	// ParamsOptional is set when the wire shape is anyOf[{$ref},null], meaning
	// the method accepts params but they may be omitted entirely.
	ParamsOptional bool
}

// MethodTable holds every method the server understands, grouped by direction.
type MethodTable struct {
	ClientRequests      []MethodEntry // client -> server, expects a result
	ServerRequests      []MethodEntry // server -> client, we must answer
	ServerNotifications []MethodEntry // server -> client, one-way
	ClientNotifications []MethodEntry // client -> server, one-way
}

// resultOverrides records every method whose result type is not simply its
// params type with "Params" replaced by "Response". The convention covers 91 of
// 95 client requests; these are the exceptions, plus the methods that take no
// params and therefore have no name to derive from.
//
// This table is the one hand-maintained artifact in the pipeline, because the
// schema describes method-to-params but contains no result mapping at all. A
// wrong entry decodes a response into the wrong struct, so validate() checks
// every entry against the schema and fails the build when either side drifts.
var resultOverrides = map[string]string{
	"initialize":                               "InitializeResponse",
	"config/value/write":                       "ConfigWriteResponse",
	"config/batchWrite":                        "ConfigWriteResponse",
	"fuzzyFileSearch":                          "FuzzyFileSearchResponse",
	"config/mcpServer/reload":                  "McpServerRefreshResponse",
	"windowsSandbox/readiness":                 "WindowsSandboxReadinessResponse",
	"account/logout":                           "LogoutAccountResponse",
	"account/rateLimits/read":                  "GetAccountRateLimitsResponse",
	"account/usage/read":                       "GetAccountTokenUsageResponse",
	"account/workspaceMessages/read":           "GetWorkspaceMessagesResponse",
	"account/read":                             "GetAccountResponse",
	"configRequirements/read":                  "ConfigRequirementsReadResponse",
	"externalAgentConfig/import/readHistories": "ExternalAgentConfigImportHistoriesReadResponse",
	"app/read":                                 "AppsReadResponse",
	"app/list":                                 "AppsListResponse",
	"app/installed":                            "AppsInstalledResponse",
	"mcpServerStatus/list":                     "ListMcpServerStatusResponse",

	// Experimental methods (present only with --experimental) whose result type
	// cannot be derived because they take no params.
	"memory/reset":              "MemoryResetResponse",
	"remoteControl/status/read": "RemoteControlStatusReadResponse",
	"remoteControl/enable":      "RemoteControlEnableResponse",
	"remoteControl/disable":     "RemoteControlDisableResponse",
}

// emptyResults are methods that answer with an empty object. They have no
// generated result type; the client returns no value beyond an error.
var emptyResults = map[string]bool{}

// loadMethods reads the four wire-union files. Each is a JSON Schema oneOf whose
// variants pair a single-valued method enum with a $ref to the payload.
func loadMethods(dir string) (*MethodTable, error) {
	t := &MethodTable{}
	for _, spec := range []struct {
		file string
		dst  *[]MethodEntry
	}{
		{"ClientRequest.json", &t.ClientRequests},
		{"ServerRequest.json", &t.ServerRequests},
		{"ServerNotification.json", &t.ServerNotifications},
		{"ClientNotification.json", &t.ClientNotifications},
	} {
		entries, err := parseMethodUnion(filepath.Join(dir, spec.file))
		if err != nil {
			return nil, err
		}
		*spec.dst = entries
	}

	// Only client requests carry results; notifications are one-way and server
	// requests are answered with a type named for the request, not the method.
	for i := range t.ClientRequests {
		e := &t.ClientRequests[i]
		if override, ok := resultOverrides[e.Method]; ok {
			e.Result = override
			continue
		}
		if e.Params == "" {
			return nil, fmt.Errorf(
				"method %q takes no params and has no entry in resultOverrides: "+
					"its result type cannot be derived, add it explicitly", e.Method)
		}
		if !strings.HasSuffix(e.Params, "Params") {
			return nil, fmt.Errorf(
				"method %q has params type %q which does not end in \"Params\": "+
					"its result type cannot be derived, add it to resultOverrides",
				e.Method, e.Params)
		}
		e.Result = strings.TrimSuffix(e.Params, "Params") + "Response"
	}
	for i := range t.ServerRequests {
		e := &t.ServerRequests[i]
		if strings.HasSuffix(e.Params, "Params") {
			e.Result = strings.TrimSuffix(e.Params, "Params") + "Response"
		}
	}
	return t, nil
}

func parseMethodUnion(path string) ([]MethodEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root Schema
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []MethodEntry
	for _, v := range root.OneOf {
		m, ok := v.Properties["method"]
		if !ok || len(m.Enum) != 1 {
			return nil, fmt.Errorf("%s: variant %q has no single-valued method enum", path, v.Title)
		}
		name, ok := m.Enum[0].(string)
		if !ok {
			return nil, fmt.Errorf("%s: variant %q has a non-string method", path, v.Title)
		}
		e := MethodEntry{Method: name, GoName: methodGoName(name)}
		if p, ok := v.Properties["params"]; ok {
			// Three shapes occur: a plain $ref (params required), an
			// anyOf[{$ref},null] (params optional), and {"type":"null"} (the
			// method takes no params at all).
			if ref, ok := p.deref(); ok {
				e.Params = ref
				e.ParamsOptional = p.nullable()
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method < out[j].Method })
	return out, nil
}

// validate fails the build when the hand-maintained result table and the
// generated schema disagree in either direction: a params or result type that
// does not exist, or an override for a method the schema no longer has. This is
// what makes a codex version bump break loudly instead of silently producing an
// untyped call.
func (t *MethodTable) validate(defs map[string]*Schema) error {
	var problems []string

	known := map[string]bool{}
	all := append(append([]MethodEntry{}, t.ClientRequests...), t.ServerRequests...)
	all = append(append(all, t.ServerNotifications...), t.ClientNotifications...)
	for _, e := range all {
		known[e.Method] = true
		if e.Params != "" && defs[e.Params] == nil {
			problems = append(problems, fmt.Sprintf(
				"method %q references params type %q which is not in the schema", e.Method, e.Params))
		}
		if e.Result != "" && !emptyResults[e.Method] && defs[e.Result] == nil {
			problems = append(problems, fmt.Sprintf(
				"method %q expects result type %q which is not in the schema "+
					"(fix or add an entry to resultOverrides)", e.Method, e.Result))
		}
	}
	for method := range resultOverrides {
		if !known[method] {
			problems = append(problems, fmt.Sprintf(
				"resultOverrides has an entry for %q, which no longer exists in the schema "+
					"(remove it)", method))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("method table is out of sync with the schema:\n  - %s",
			strings.Join(problems, "\n  - "))
	}
	return nil
}

// methodGoName converts a wire method name into a Go identifier:
// "thread/start" -> "ThreadStart", "item/agentMessage/delta" -> "ItemAgentMessageDelta",
// "thread/inject_items" -> "ThreadInjectItems".
func methodGoName(method string) string {
	var b strings.Builder
	for _, segment := range strings.FieldsFunc(method, func(r rune) bool {
		return r == '/' || r == '_' || r == '-' || r == '.'
	}) {
		b.WriteString(exportedIdent(segment))
	}
	return b.String()
}
