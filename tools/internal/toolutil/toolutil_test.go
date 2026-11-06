package toolutil_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

func TestDecodeArgs(t *testing.T) {
	t.Run("empty arguments decode as empty object", func(t *testing.T) {
		var got struct {
			Name string `json:"name"`
		}
		for _, arguments := range []string{"", "  \n\t ", "{}"} {
			if err := toolutil.DecodeArgs(rig.ToolCall{Arguments: arguments}, &got); err != nil {
				t.Fatalf("DecodeArgs(%q): %v", arguments, err)
			}
			if got.Name != "" {
				t.Fatalf("DecodeArgs(%q) = %+v, want zero value", arguments, got)
			}
		}
	})

	t.Run("arguments decode", func(t *testing.T) {
		var got struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		if err := toolutil.DecodeArgs(rig.ToolCall{Arguments: ` {"name":"x","count":2} `}, &got); err != nil {
			t.Fatalf("DecodeArgs: %v", err)
		}
		if got.Name != "x" || got.Count != 2 {
			t.Fatalf("DecodeArgs = %+v, want {x 2}", got)
		}
	})

	t.Run("malformed arguments carry the historical message", func(t *testing.T) {
		err := toolutil.DecodeArgs(rig.ToolCall{Arguments: "{"}, &struct{}{})
		if err == nil {
			t.Fatal("DecodeArgs accepted malformed JSON")
		}
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Fatalf("error %v does not wrap the JSON error", err)
		}
		if !strings.HasPrefix(err.Error(), "invalid arguments JSON: ") {
			t.Fatalf("error = %q, want the invalid arguments JSON prefix", err)
		}
	})
}

func TestDecodeArgsAs(t *testing.T) {
	err := toolutil.DecodeArgsAs("ralph", rig.ToolCall{Arguments: "{"}, &struct{}{})
	if err == nil {
		t.Fatal("DecodeArgsAs accepted malformed JSON")
	}
	const want = "ralph: invalid arguments JSON: "
	if !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %q, want prefix %q", err, want)
	}
	// The qualifier must not leak into the plain helper.
	if err := toolutil.DecodeArgs(rig.ToolCall{Arguments: "{"}, &struct{}{}); strings.Contains(err.Error(), "ralph:") {
		t.Fatalf("DecodeArgs error %q carries a qualifier", err)
	}
}

func TestResults(t *testing.T) {
	if got := toolutil.TextResult("ok"); got.Content != "ok" || got.IsError {
		t.Fatalf("TextResult = %+v, want content ok and IsError false", got)
	}
	base := errors.New("boom")
	if got := toolutil.ErrorResult(base); got.Content != "boom" || !got.IsError {
		t.Fatalf("ErrorResult = %+v, want content boom and IsError true", got)
	}
	if got := toolutil.ErrorResultf("%s: %d", "x", 2); got.Content != "x: 2" || !got.IsError {
		t.Fatalf("ErrorResultf = %+v, want content %q and IsError true", got, "x: 2")
	}
}

func TestSchemaBuilders(t *testing.T) {
	stringProp := toolutil.StringProp("a string")
	if stringProp["type"] != "string" || stringProp["description"] != "a string" {
		t.Fatalf("StringProp = %+v", stringProp)
	}
	integerProp := toolutil.IntegerProp("an integer")
	if integerProp["type"] != "integer" || integerProp["description"] != "an integer" {
		t.Fatalf("IntegerProp = %+v", integerProp)
	}
	booleanProp := toolutil.BooleanProp("a boolean")
	if booleanProp["type"] != "boolean" || booleanProp["description"] != "a boolean" {
		t.Fatalf("BooleanProp = %+v", booleanProp)
	}
	arrayProp := toolutil.StringArrayProp("an array")
	items, ok := arrayProp["items"].(map[string]any)
	if arrayProp["type"] != "array" || !ok || items["type"] != "string" {
		t.Fatalf("StringArrayProp = %+v", arrayProp)
	}

	properties := map[string]any{"name": stringProp}
	schema := toolutil.ObjectSchema(properties, "name")
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("ObjectSchema = %+v", schema)
	}
	if got, ok := schema["properties"].(map[string]any); !ok || len(got) != 1 {
		t.Fatalf("ObjectSchema properties = %+v", schema["properties"])
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "name" {
		t.Fatalf("ObjectSchema required = %+v", schema["required"])
	}
	if _, ok := toolutil.ObjectSchema(properties)["required"]; ok {
		t.Fatal("ObjectSchema without required names must omit the required key")
	}
}

func TestTruncateBytes(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		max    int
		suffix string
		want   string
	}{
		{"short input untouched", "hello", 10, "…", "hello"},
		{"exact fit untouched", "hello", 5, "…", "hello"},
		{"non-positive max is a no-op", "hello", 0, "…", "hello"},
		{"negative max is a no-op", "hello", -1, "…", "hello"},
		{"ascii cut", "hello", 3, "…", "hel…"},
		{"multibyte rune boundary", "héllo", 2, "…", "h…"},
		{"multibyte rune not split", "héllo", 1, "…", "h…"},
		{"emoji rune boundary", "a😀b", 2, "…", "a…"},
		{"empty suffix permitted", "hello", 2, "", "he"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolutil.TruncateBytes(tc.input, tc.max, tc.suffix); got != tc.want {
				t.Fatalf("TruncateBytes(%q, %d, %q) = %q, want %q", tc.input, tc.max, tc.suffix, got, tc.want)
			}
		})
	}

	// The length contract is about bytes, not runes: the result is at most max
	// bytes plus the suffix, and it never ends inside a UTF-8 sequence.
	long := strings.Repeat("é", 100)
	got := toolutil.TruncateBytes(long, 51, "…")
	if len(got) > 51+len("…") {
		t.Fatalf("truncated length = %d bytes, want <= %d", len(got), 51+len("…"))
	}
	if !strings.ContainsRune(got, 'é') {
		t.Fatalf("truncated value lost all runes: %q", got)
	}
}
