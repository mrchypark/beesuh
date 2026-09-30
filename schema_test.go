package beesuh

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestOutputContract(t *testing.T) {
	f := newFakeOpenCode(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/message") {
			fmt.Fprint(w, `{"info":{"id":"msg_answer","sessionID":"ses_1","role":"assistant","time":{"completed":1},"finish":"stop","structured":{"answer":42}},"parts":[]}`)
			return
		}
		f.serveHTTP(w, r)
	}))
	defer upstream.Close()
	r, err := Open(context.Background(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, OpenCode: opencode.Config{BaseURL: upstream.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	req := testRequest("s", "invalid-output", "answer")
	req.Prompt.Schema = []byte(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	out, err := r.Run(context.Background(), req)
	if err == nil || out.State != "failed" || len(out.Output) != 0 {
		t.Fatalf("schema mismatch accepted: %+v %v", out, err)
	}
	req.RequestID = "valid-output"
	req.Prompt.Schema = []byte(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)
	out, err = r.Run(context.Background(), req)
	if err != nil || out.State != "succeeded" || string(out.Output) != `{"answer":42}` {
		t.Fatalf("valid output rejected: %+v %v", out, err)
	}
}

func TestSchemaPreservesIntegerPrecisionAndRejectsExternalReferences(t *testing.T) {
	schema, err := compileSchema([]byte(`{"const":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewBufferString(`9007199254740992`))
	if err != nil {
		t.Fatal(err)
	}
	if schema.Validate(value) == nil {
		t.Fatal("rounded integer passed const validation")
	}
	if _, err := compileSchema([]byte(`{"$ref":"https://example.invalid/schema"}`)); err == nil {
		t.Fatal("external schema accepted")
	}
}
