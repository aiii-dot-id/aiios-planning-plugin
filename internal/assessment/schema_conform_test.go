package assessment

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

func conform(schema map[string]any, v any, at string) error {
	if c, ok := schema["const"]; ok && fmt.Sprint(c) != fmt.Sprint(v) {
		return fmt.Errorf("%s: %v is not the constant %v", at, v, c)
	}
	if e, ok := schema["enum"].([]any); ok {
		found := false
		for _, x := range e {
			found = found || fmt.Sprint(x) == fmt.Sprint(v)
		}
		if !found {
			return fmt.Errorf("%s: %v is not in its enum", at, v)
		}
	}
	types := []string{}
	switch t := schema["type"].(type) {
	case string:
		types = append(types, t)
	case []any:
		for _, x := range t {
			types = append(types, x.(string))
		}
	}
	if len(types) == 0 {
		return nil
	}
	kind := ""
	switch x := v.(type) {
	case nil:
		kind = "null"
	case bool:
		kind = "boolean"
	case string:
		kind = "string"
		if n, ok := schema["minLength"].(float64); ok && utf8.RuneCountInString(x) < int(n) {
			return fmt.Errorf("%s: shorter than %v", at, n)
		}
		if n, ok := schema["maxLength"].(float64); ok && utf8.RuneCountInString(x) > int(n) {
			return fmt.Errorf("%s: longer than %v", at, n)
		}
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			return fmt.Errorf("%s: %q does not match %s", at, x, p)
		}
	case float64:
		kind = "number"
		if x == math.Trunc(x) {
			kind = "integer"
		}
		if n, ok := schema["minimum"].(float64); ok && x < n {
			return fmt.Errorf("%s: below %v", at, n)
		}
		if n, ok := schema["maximum"].(float64); ok && x > n {
			return fmt.Errorf("%s: above %v", at, n)
		}
	case []any:
		kind = "array"
		if n, ok := schema["maxItems"].(float64); ok && len(x) > int(n) {
			return fmt.Errorf("%s: more than %v items", at, n)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range x {
				if err := conform(items, item, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		kind = "object"
		props, _ := schema["properties"].(map[string]any)
		for k, val := range x {
			sub, listed := props[k].(map[string]any)
			if !listed {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("%s: %q is not a property the schema allows", at, k)
				}
				continue
			}
			if err := conform(sub, val, at+"."+k); err != nil {
				return err
			}
		}
		if req, ok := schema["required"].([]any); ok {
			for _, k := range req {
				if _, ok := x[k.(string)]; !ok {
					return fmt.Errorf("%s: required %q is missing", at, k)
				}
			}
		}
	}
	for _, t := range types {
		if t == kind || t == "number" && kind == "integer" {
			return nil
		}
	}
	return fmt.Errorf("%s: a %s where the schema says %s", at, kind, strings.Join(types, " or "))
}

func TestReviewOutputConformsToItsPackagedSchema(t *testing.T) {
	raw, err := os.ReadFile("../../schemas/review_out.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	f := newFixture(sample("failed"), 1)
	r := f.rows[5]
	r.Content = "→ shell(" + strings.Repeat("argument", 80) + ")\n← prerequisite missing\n[exit status 1]"
	f.rows[5] = r
	for i := 0; i < 20; i++ {
		f.rows = append(f.rows, sdk.InteractionRecord{ID: fmt.Sprintf("later-%d", i), Sequence: uint64(7 + i), Kind: sdk.InteractionNotice, ProjectID: "project", TurnID: "turn", Details: sdk.InteractionDetails{WorkSession: "ws"}})
	}
	reader := &detailFixture{fixture: f, full: r}
	for _, view := range []string{"compact", "full"} {
		q := DefaultReviewRequest("ws")
		q.View, q.RecordLimit = view, 512
		got, err := Review(reader, q)
		if err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"result_detail_request"`) {
			t.Fatalf("the %s review offers no detail request to hold to the schema: %s", view, out)
		}
		var v any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatal(err)
		}
		if err := conform(schema, v, view); err != nil {
			t.Fatalf("the %s review is refused by its own output schema: %v", view, err)
		}
	}
}
