package cli

import (
	"errors"
	"testing"
)

// Golden tests for the single renderer (render.go): both modes, plus the
// error envelope and the ParseFormat contract. Text goldens are inline
// string literals so they fail loudly on any format drift.

func TestRenderEnvelopeGolden(t *testing.T) {
	t.Parallel()
	env := Envelope{
		SchemaVersion: JSONSchemaVersion,
		Command:       "version",
		Message:       "ok",
		Data: map[string]any{
			"minDokku": MinDokkuVersion,
			"topctl":   "v0.0.0",
		},
	}

	var b stringsBuffer
	if err := RenderEnvelope(&b, FormatJSON, env); err != nil {
		t.Fatalf("json: %v", err)
	}
	wantJSON := `{"schemaVersion":1,"command":"version","message":"ok","data":{"minDokku":"0.38","topctl":"v0.0.0"}}` + "\n"
	if b.String() != wantJSON {
		t.Errorf("json golden:\n got %q\nwant %q", b.String(), wantJSON)
	}

	b = stringsBuffer{}
	if err := RenderEnvelope(&b, FormatText, env); err != nil {
		t.Fatalf("text: %v", err)
	}
	wantText := "version\n  message: ok\n  minDokku: 0.38\n  topctl: v0.0.0\n"
	if b.String() != wantText {
		t.Errorf("text golden:\n got %q\nwant %q", b.String(), wantText)
	}
}

func TestRenderErrEnvelopeGolden(t *testing.T) {
	t.Parallel()
	var env ErrEnvelope
	env.SchemaVersion = JSONSchemaVersion
	env.Command = "deploy"
	env.Error.Code = "usage"
	env.Error.Message = "bad service reference"

	var b stringsBuffer
	if err := RenderErrEnvelope(&b, FormatJSON, env); err != nil {
		t.Fatalf("json: %v", err)
	}
	wantJSON := `{"schemaVersion":1,"command":"deploy","error":{"code":"usage","message":"bad service reference"}}` + "\n"
	if b.String() != wantJSON {
		t.Errorf("json golden:\n got %q\nwant %q", b.String(), wantJSON)
	}

	b = stringsBuffer{}
	if err := RenderErrEnvelope(&b, FormatText, env); err != nil {
		t.Fatalf("text: %v", err)
	}
	wantText := "topctl deploy: bad service reference (code: usage)\n"
	if b.String() != wantText {
		t.Errorf("text golden:\n got %q\nwant %q", b.String(), wantText)
	}
}

func TestParseFormat(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Format{"text": FormatText, "json": FormatJSON} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseFormat("yaml"); !errors.Is(err, ErrUsage) {
		t.Errorf("ParseFormat(%q) err = %v, want ErrUsage-wrapped", "yaml", err)
	}
}
