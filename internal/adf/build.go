package adf

import "encoding/json"

// ---- ADF construction (write path) -----------------------------------------
//
// This file holds the *construction* helpers for Atlassian Document Format
// (ADF) documents — the inverse direction of the read path that lives in
// adf.go (Walk, ExtractLinks, RenderMarkdown). Callers supplying plain-text
// issue descriptions or comments use these helpers to produce the ADF JSON
// the Jira Cloud v3 write API requires.
//
// The two paths are kept in separate files but the same package because they
// model the same on-the-wire format. They deliberately use DIFFERENT internal
// struct types: the read-side [Node] does not carry the top-level "version"
// field (it represents an arbitrary node, not necessarily a document root)
// and reusing it here would either omit "version" or pollute every other
// Node with a write-only field. The tiny duplication below is the smaller
// cost.

// adfDoc is the top-level ADF document envelope. Field ordering in the
// generated JSON matches the canonical shape documented at
// https://developer.atlassian.com/cloud/jira/platform/apis/document/structure/.
type adfDoc struct {
	Version int        `json:"version"`
	Type    string     `json:"type"`
	Content []adfBlock `json:"content"`
}

// adfBlock is a block-level node (paragraph, heading, …). For the
// single-paragraph constructor below only "paragraph" is used, but the
// type is shaped to be reusable should richer constructors be added in
// future. Content uses omitempty so a paragraph with no children
// serializes as {"type":"paragraph"} — the canonical empty-paragraph
// form, and what the Phase-2 PRD locks in for the empty-text case.
type adfBlock struct {
	Type    string      `json:"type"`
	Content []adfInline `json:"content,omitempty"`
}

// adfInline is an inline node (text, hardBreak, …). For BuildParagraphDoc
// only "text" with a Text payload is emitted.
type adfInline struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// BuildParagraphDoc builds a minimal valid Atlassian Document Format (ADF)
// document containing a single paragraph with the given text. It is the
// inverse direction of the package's read path (Walk/RenderMarkdown):
// callers supplying plain-text issue descriptions or comments use this to
// produce the ADF JSON the Jira Cloud v3 write API requires.
//
// The returned document has the canonical shape:
//
//	{"version":1,"type":"doc","content":[
//	  {"type":"paragraph","content":[{"type":"text","text":"<text>"}]}
//	]}
//
// When text is empty the paragraph is emitted with its content array
// omitted ({"type":"paragraph"}), which is still a valid ADF doc, so
// callers can create an empty-bodied comment/description deliberately.
//
// It is a pure function: no I/O, no side effects. It never returns an
// error because the structure is fixed and json.Marshal of these typed
// values cannot fail; the byte slice is returned as a [json.RawMessage]
// so callers can embed it directly in larger JSON bodies.
func BuildParagraphDoc(text string) json.RawMessage {
	para := adfBlock{Type: "paragraph"}
	if text != "" {
		para.Content = []adfInline{{Type: "text", Text: text}}
	}
	doc := adfDoc{
		Version: 1,
		Type:    "doc",
		Content: []adfBlock{para},
	}
	// json.Marshal on these concrete value types cannot fail; ignore err
	// to keep the function's pure-value signature.
	b, _ := json.Marshal(doc)
	return json.RawMessage(b)
}
