package adf_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/neumachen/gojira/internal/adf"
)

// canonicalNonEmpty is the exact shape BuildParagraphDoc must emit for a
// non-empty text input. Kept as a literal so the test fails loudly if the
// builder ever drifts (extra fields, wrong types, missing keys).
const canonicalNonEmpty = `{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Hello world"}]}]}`

// canonicalEmpty is the exact shape for the empty-text case: a doc with a
// single paragraph that has no content array. Both an absent "content" field
// and an empty array (`[]`) are valid ADF; we lock in "absent" because that
// is what omitempty produces and what the spec example shows.
const canonicalEmpty = `{"version":1,"type":"doc","content":[{"type":"paragraph"}]}`

// jsonEqual returns true when a and b decode to the same JSON value
// regardless of whitespace or key ordering.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("invalid JSON (a): %v\nbytes: %s", err, string(a))
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("invalid JSON (b): %v\nbytes: %s", err, string(b))
	}
	return reflect.DeepEqual(av, bv)
}

func TestBuildParagraphDoc_Basic(t *testing.T) {
	t.Parallel()

	got := adf.BuildParagraphDoc("Hello world")
	if !json.Valid(got) {
		t.Fatalf("BuildParagraphDoc returned invalid JSON: %s", string(got))
	}
	want := []byte(canonicalNonEmpty)
	if !jsonEqual(t, got, want) {
		t.Errorf("BuildParagraphDoc(non-empty) shape mismatch\n got: %s\nwant: %s",
			string(got), string(want))
	}
}

func TestBuildParagraphDoc_Empty(t *testing.T) {
	t.Parallel()

	got := adf.BuildParagraphDoc("")
	if !json.Valid(got) {
		t.Fatalf("BuildParagraphDoc(\"\") returned invalid JSON: %s", string(got))
	}
	want := []byte(canonicalEmpty)
	if !jsonEqual(t, got, want) {
		t.Errorf("BuildParagraphDoc(\"\") shape mismatch\n got: %s\nwant: %s",
			string(got), string(want))
	}
}

// TestBuildParagraphDoc_RoundTrip proves the builder output is consumable by
// the existing read path: feeding the produced doc into adf.RenderMarkdown
// must yield Markdown that contains the original text.
func TestBuildParagraphDoc_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []string{
		"Hello world",
		"single line of plain text",
		"text with numbers 123 and symbols & * (parens)",
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			doc := adf.BuildParagraphDoc(in)

			md, unknowns, err := adf.RenderMarkdown(doc)
			if err != nil {
				t.Fatalf("RenderMarkdown: %v", err)
			}
			if len(unknowns) != 0 {
				t.Errorf("expected zero unknown nodes, got %d: %+v", len(unknowns), unknowns)
			}
			if !strings.Contains(md, in) {
				t.Errorf("rendered Markdown must contain the original text\n input: %q\n output: %q", in, md)
			}
		})
	}
}

// TestBuildParagraphDoc_Escaping confirms the builder produces valid JSON
// for inputs containing characters JSON must escape (quote, backslash,
// newline, tab) and for non-ASCII unicode, and that the existing reader
// round-trips the text through RenderMarkdown.
func TestBuildParagraphDoc_Escaping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		text string
	}{
		{"double quote", `she said "hi"`},
		{"backslash", `path\to\thing`},
		{"newline", "line one\nline two"},
		{"tab", "col1\tcol2"},
		{"unicode bmp", "résumé — naïve"},
		{"unicode emoji", "ship it 🚀"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := adf.BuildParagraphDoc(tc.text)
			if !json.Valid(doc) {
				t.Fatalf("invalid JSON: %s", string(doc))
			}

			// Round-trip the text via the reader. A newline inside a
			// single text node renders verbatim into the Markdown
			// output (no paragraph split), so strings.Contains is
			// safe for every fixture above.
			md, unknowns, err := adf.RenderMarkdown(doc)
			if err != nil {
				t.Fatalf("RenderMarkdown: %v", err)
			}
			if len(unknowns) != 0 {
				t.Errorf("expected zero unknown nodes, got %d: %+v", len(unknowns), unknowns)
			}
			if !strings.Contains(md, tc.text) {
				t.Errorf("rendered Markdown must contain the original text\n input: %q\n output: %q",
					tc.text, md)
			}
		})
	}
}
