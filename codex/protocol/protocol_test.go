package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/ccheers/codexadkv2/codex/protocol"
)

// TestInternallyTaggedRoundTrip checks that a tagged union decodes into the
// right payload and re-encodes to equivalent JSON.
func TestInternallyTaggedRoundTrip(t *testing.T) {
	const in = `{"type":"workspaceWrite","writableRoots":["/w"],"networkAccess":true,` +
		`"excludeTmpdirEnvVar":false,"excludeSlashTmp":false}`

	var p protocol.SandboxPolicy
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != protocol.SandboxPolicyWorkspaceWrite {
		t.Fatalf("Type = %q, want workspaceWrite", p.Type)
	}
	ww, ok := p.AsWorkspaceWrite()
	if !ok {
		t.Fatal("AsWorkspaceWrite returned false")
	}
	if len(ww.WritableRoots) != 1 || ww.WritableRoots[0] != "/w" {
		t.Errorf("WritableRoots = %v, want [/w]", ww.WritableRoots)
	}
	if ww.NetworkAccess == nil || !*ww.NetworkAccess {
		t.Errorf("NetworkAccess = %v, want true", ww.NetworkAccess)
	}
	// A non-matching accessor must report false rather than panic.
	if _, ok := p.AsReadOnly(); ok {
		t.Error("AsReadOnly returned true on a workspaceWrite value")
	}

	assertJSONEqual(t, &p, in)
}

// TestExternallyTaggedBareString covers the string arm of an externally tagged
// union: "contextWindowExceeded" rather than an object.
func TestExternallyTaggedBareString(t *testing.T) {
	var e protocol.CodexErrorInfo
	if err := json.Unmarshal([]byte(`"contextWindowExceeded"`), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Kind != protocol.CodexErrorInfoContextWindowExceeded {
		t.Fatalf("Kind = %q, want contextWindowExceeded", e.Kind)
	}
	assertJSONEqual(t, &e, `"contextWindowExceeded"`)
}

// TestExternallyTaggedObject covers the single-key object arm of the same union.
func TestExternallyTaggedObject(t *testing.T) {
	const in = `{"httpConnectionFailed":{"httpStatusCode":503}}`

	var e protocol.CodexErrorInfo
	if err := json.Unmarshal([]byte(in), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Kind != protocol.CodexErrorInfoHTTPConnectionFailed {
		t.Fatalf("Kind = %q, want httpConnectionFailed", e.Kind)
	}
	p, ok := e.AsHTTPConnectionFailed()
	if !ok {
		t.Fatal("AsHTTPConnectionFailed returned false")
	}
	if p.HTTPStatusCode == nil || *p.HTTPStatusCode != 503 {
		t.Errorf("HTTPStatusCode = %v, want 503", p.HTTPStatusCode)
	}
	assertJSONEqual(t, &e, in)
}

// TestUnknownVariantPreserved is the forward-compatibility guarantee: a variant
// this build has never heard of must decode without error and re-encode
// unchanged, because the SDK is expected to run against newer servers.
func TestUnknownVariantPreserved(t *testing.T) {
	const in = `{"type":"quantumSandbox","qubits":7}`

	var p protocol.SandboxPolicy
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatalf("unknown variant must not error, got: %v", err)
	}
	if p.Type != "quantumSandbox" {
		t.Errorf("Type = %q, want the unknown tag preserved", p.Type)
	}
	if _, ok := p.AsWorkspaceWrite(); ok {
		t.Error("AsWorkspaceWrite returned true for an unknown variant")
	}
	// Round-tripping must not lose the unrecognized "qubits" field.
	assertJSONEqual(t, &p, in)
}

// TestUnknownEnumValuePreserved is the same guarantee for enums.
func TestUnknownEnumValuePreserved(t *testing.T) {
	var s protocol.SandboxMode
	if err := json.Unmarshal([]byte(`"quantum-write"`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s != "quantum-write" {
		t.Errorf("value = %q, want it preserved verbatim", s)
	}
	if s.IsKnown() {
		t.Error("IsKnown() = true for a value this build does not define")
	}
	if protocol.SandboxModeReadOnly.IsKnown() != true {
		t.Error("IsKnown() = false for a known value")
	}
}

// TestSandboxModeWireValues guards the trap where the same concept has two
// spellings: thread/start takes kebab-case SandboxMode, turn/start takes
// camelCase SandboxPolicy tags. Deriving either from the other breaks the wire.
func TestSandboxModeWireValues(t *testing.T) {
	if got := string(protocol.SandboxModeReadOnly); got != "read-only" {
		t.Errorf("SandboxModeReadOnly = %q, want read-only", got)
	}
	if got := string(protocol.SandboxPolicyReadOnly); got != "readOnly" {
		t.Errorf("SandboxPolicyReadOnly = %q, want readOnly", got)
	}
}

// TestTriStateNullable covers the three states of a tri-state field. A plain
// pointer cannot express "explicitly null", which is what clears the value.
func TestTriStateNullable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *protocol.Nullable[string]
		want  string
	}{
		{"absent", nil, `{"threadId":"t","input":null}`},
		{"explicit null", protocol.Null[string](), `{"threadId":"t","input":null,"serviceTier":null}`},
		{"value", protocol.Value("flex"), `{"threadId":"t","input":null,"serviceTier":"flex"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := protocol.TurnStartParams{ThreadID: "t", ServiceTier: tc.value}
			got, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			assertJSONEqualBytes(t, got, tc.want)
		})
	}
}

// TestRequiredNullableEmitsNull checks that a field which is both required and
// nullable serializes as an explicit null rather than being omitted, since
// omitting it would change the message.
//
// Only three fields in the whole protocol have this shape; Thread.projectId is
// one. Everything else that is nullable is also optional, and those correctly
// use omitempty.
func TestRequiredNullableEmitsNull(t *testing.T) {
	got, err := json.Marshal(protocol.Thread{ID: "thr_1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := fields["projectId"]
	if !ok {
		t.Fatal("projectId was omitted; a required-nullable field must serialize as null")
	}
	if string(raw) != "null" {
		t.Errorf("projectId = %s, want null", raw)
	}
}

// TestOptionalNullableIsOmitted is the complementary case: a field that is
// nullable but NOT required must be omitted when unset, not sent as null.
func TestOptionalNullableIsOmitted(t *testing.T) {
	got, err := json.Marshal(protocol.TurnError{Message: "boom"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["codexErrorInfo"]; ok {
		t.Error("codexErrorInfo was sent; an optional field must be omitted when unset")
	}
	if _, ok := fields["message"]; !ok {
		t.Error("message was omitted; a required field must always be sent")
	}
}

// TestThreadItemVariants spot-checks the largest union in the protocol.
func TestThreadItemVariants(t *testing.T) {
	const in = `{"type":"agentMessage","id":"i1","text":"hi","phase":null,` +
		`"memoryCitation":null,"delivery":null}`

	var item protocol.ThreadItem
	if err := json.Unmarshal([]byte(in), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg, ok := item.AsAgentMessage()
	if !ok {
		t.Fatal("AsAgentMessage returned false")
	}
	if msg.Text != "hi" || msg.ID != "i1" {
		t.Errorf("got id=%q text=%q, want i1/hi", msg.ID, msg.Text)
	}
}

// TestMarshalDrivenByTag verifies that marshaling follows the discriminant and
// ignores a payload that does not match it, so an inconsistently built value
// cannot put a malformed message on the wire.
func TestMarshalDrivenByTag(t *testing.T) {
	p := protocol.SandboxPolicy{
		Type:     protocol.SandboxPolicyDangerFullAccess,
		ReadOnly: &protocol.SandboxPolicyReadOnlyPayload{NetworkAccess: ptr(true)},
	}
	got, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertJSONEqualBytes(t, got, `{"type":"dangerFullAccess"}`)
}

// TestConstructorsSetTagAndPayload checks the write path.
func TestConstructorsSetTagAndPayload(t *testing.T) {
	p := protocol.NewSandboxPolicyWorkspaceWrite(protocol.SandboxPolicyWorkspaceWritePayload{
		WritableRoots: []protocol.AbsolutePathBuf{"/repo"},
		NetworkAccess: ptr(false),
	})
	if p.Type != protocol.SandboxPolicyWorkspaceWrite {
		t.Fatalf("Type = %q", p.Type)
	}
	if _, ok := p.AsWorkspaceWrite(); !ok {
		t.Error("constructor did not set the payload matching the tag")
	}
}

// TestNotificationParamsCoversEveryMethod asserts the generated dispatch table
// resolves a params type for every server notification. A nil here means the
// dispatcher would silently fall back to raw JSON for a method we do know.
func TestNotificationParamsCoversEveryMethod(t *testing.T) {
	for _, method := range []string{
		protocol.NotifyThreadStarted,
		protocol.NotifyTurnStarted,
		protocol.NotifyTurnCompleted,
		protocol.NotifyItemStarted,
		protocol.NotifyItemCompleted,
		protocol.NotifyItemAgentMessageDelta,
		protocol.NotifyThreadStatusChanged,
		protocol.NotifyError,
	} {
		if protocol.NotificationParams(method) == nil {
			t.Errorf("NotificationParams(%q) = nil, want a typed params value", method)
		}
	}
	if protocol.NotificationParams("thread/notAThing") != nil {
		t.Error("an unknown method must resolve to nil so the caller can surface raw JSON")
	}
}

func ptr[T any](v T) *T { return &v }

func assertJSONEqual(t *testing.T, v any, want string) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertJSONEqualBytes(t, got, want)
}

// assertJSONEqualBytes compares JSON semantically, ignoring key order.
func assertJSONEqualBytes(t *testing.T, got []byte, want string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("unmarshal got %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("unmarshal want %s: %v", want, err)
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if string(ga) != string(gb) {
		t.Errorf("JSON mismatch:\n got: %s\nwant: %s", ga, gb)
	}
}

// TestTriStateFieldsAreComplete guards the one thing the JSON Schema cannot tell
// the generator.
//
// A field whose Rust type is Option<Option<T>> distinguishes "absent" from
// "explicit null", but the JSON Schema collapses both into a plain nullable. Only
// the TypeScript output preserves the distinction, as `| null | null`. So the
// generator carries a hand-maintained list, and a missing entry is a silent bug:
// the field compiles as *T and callers simply have no way to clear it.
//
// This test pins the fields currently known to need Nullable[T]. If a schema
// upgrade adds another, re-derive the list with:
//
//	codex app-server generate-ts --out /tmp/codex-ts --experimental
//	grep -rl "null | null" /tmp/codex-ts
func TestTriStateFieldsAreComplete(t *testing.T) {
	// Each of these must be *Nullable[T], not a plain pointer.
	assertNullable := func(name string, field any) {
		t.Helper()
		if _, ok := field.(*protocol.Nullable[string]); !ok {
			t.Errorf("%s is %T, want *protocol.Nullable[string]: a plain pointer "+
				"cannot express \"clear this value\"", name, field)
		}
	}

	assertNullable("ThreadStartParams.ServiceTier", protocol.ThreadStartParams{}.ServiceTier)
	assertNullable("ThreadResumeParams.ServiceTier", protocol.ThreadResumeParams{}.ServiceTier)
	assertNullable("ThreadForkParams.ServiceTier", protocol.ThreadForkParams{}.ServiceTier)
	assertNullable("TurnStartParams.ServiceTier", protocol.TurnStartParams{}.ServiceTier)
	assertNullable("ThreadSettingsUpdateParams.ServiceTier", protocol.ThreadSettingsUpdateParams{}.ServiceTier)
	assertNullable("ThreadRealtimeStartParams.Prompt", protocol.ThreadRealtimeStartParams{}.Prompt)
}
