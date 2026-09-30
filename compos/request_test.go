package compos

import (
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	request, err := Decode(strings.NewReader(`{
		"agent_definition":{"instructions":"Return the answer."},
		"task_definition":{"input_schema":{"type":"object","required":["name"]},"output_schema":{"type":"object","required":["answer"]}},
		"input":{"name":"Ada"},"subject_ref":{"name":"Ada"},
		"_compos":{"workspace_id":"wks_1","execution_request_id":"agn_1","timeout_seconds":30,"retry_policy":{"max_attempts":1}}
	}`), "customer:1")
	if err != nil {
		t.Fatal(err)
	}
	if request.WorkspaceID != "wks_1" || request.RequestID != "agn_1" || request.SessionKey != "customer:1" {
		t.Fatalf("request identity = %+v", request)
	}
	if !strings.Contains(request.Prompt.Text, `"subject_ref":{"name":"Ada"}`) || string(request.Prompt.Schema) == "" {
		t.Fatalf("prompt = %+v", request.Prompt)
	}
}

func TestDecodeRejectsUnsupportedPolicyAndRemoteSchema(t *testing.T) {
	input := `{"agent_definition":{"instructions":"x","tools":["shell"]},"task_definition":{"input_schema":{"type":"object","$ref":"https://example.test/schema"},"output_schema":{"type":"object"}},"input":{},"subject_ref":{},"_compos":{"workspace_id":"w","execution_request_id":"r"}}`
	if _, err := Decode(strings.NewReader(input), "s"); err == nil || !strings.Contains(err.Error(), "tools is unsupported") {
		t.Fatalf("Decode error = %v", err)
	}
	input = strings.Replace(input, `,"tools":["shell"]`, "", 1)
	if _, err := Decode(strings.NewReader(input), "s"); err == nil || !strings.Contains(err.Error(), "external schema reference") {
		t.Fatalf("Decode error = %v", err)
	}
}

func TestDecodeRejectsOversizeTrailingWhitespaceAndTimeoutOverflow(t *testing.T) {
	valid := `{"agent_definition":{"instructions":"x"},"task_definition":{"input_schema":{"type":"object"},"output_schema":{"type":"object"}},"input":{},"subject_ref":{},"_compos":{"workspace_id":"w","execution_request_id":"r"}}`
	if _, err := Decode(strings.NewReader(valid+strings.Repeat(" ", maxInputBytes)), "s"); err == nil || !strings.Contains(err.Error(), "exceeds 256 KiB") {
		t.Fatalf("oversize Decode error = %v", err)
	}
	overflow := strings.Replace(valid, `"execution_request_id":"r"`, `"execution_request_id":"r","timeout_seconds":9223372037`, 1)
	if _, err := Decode(strings.NewReader(overflow), "s"); err == nil || !strings.Contains(err.Error(), "timeout_seconds") {
		t.Fatalf("overflow Decode error = %v", err)
	}
}
