// Package adf provides pure, stateless traversal and rendering of Atlassian
// Document Format (ADF) documents.
//
// ADF is the rich-text format used by Jira Cloud for issue descriptions,
// comments, and other text fields. The top-level shape is:
//
//	{"version": 1, "type": "doc", "content": [...]}
//
// Each node carries a "type" string, an optional "content" array of child
// nodes, an optional "text" string (leaf text nodes), optional "marks" (inline
// formatting), and optional "attrs" (node-specific attributes).
//
// This package has exactly one project-internal import: classify, which is
// used to label every link URL discovered during traversal. It performs no
// network I/O and no filesystem writes.
package adf

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/pkg/classify"
)

// ---- ADF wire types --------------------------------------------------------

// Node is a single node in an ADF document tree. It is exported so that
// Visitor implementations can inspect node fields.
type Node struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Content []Node          `json:"content,omitempty"`
	Marks   []mark          `json:"marks,omitempty"`
	Attrs   json.RawMessage `json:"attrs,omitempty"`
}

// mark is an inline formatting annotation attached to a text node.
type mark struct {
	Type  string          `json:"type"`
	Attrs json.RawMessage `json:"attrs,omitempty"`
}

// linkAttrs holds the href extracted from a link mark's attrs object.
type linkAttrs struct {
	Href string `json:"href"`
}

// headingAttrs holds the level extracted from a heading node's attrs object.
type headingAttrs struct {
	Level int `json:"level"`
}

// codeBlockAttrs holds the language tag from a codeBlock node's attrs object.
type codeBlockAttrs struct {
	Language string `json:"language"`
}

// ---- Public types ----------------------------------------------------------

// Link represents a single hyperlink discovered inside an ADF document.
// The URL is the raw href value; Text is the visible label from the text node
// that carries the link mark; Classification is the result of calling
// classify.Classify on the URL.
type Link struct {
	URL            string
	Text           string
	Classification classify.Result
}

// UnknownNode records an ADF node whose type the renderer does not handle.
// The raw JSON is preserved so a downstream caller can inspect or re-render it.
// The renderer itself emits a Markdown comment and preserves any inner text.
type UnknownNode struct {
	// NodeType is the value of the "type" field in the ADF JSON.
	NodeType string
	// Raw is the complete JSON of the node as it appeared in the document.
	Raw json.RawMessage
}

// ---- Visitor ---------------------------------------------------------------

// Visitor is called by Walk for each node in document order (pre-order,
// parent before children).
//
// The raw argument is the original JSON bytes for the node — useful for
// unknown-node preservation.
//
// Returning a non-nil error from Visit stops the walk and propagates the
// error to the Walk caller.
type Visitor interface {
	Visit(n *Node, raw json.RawMessage) error
}

// VisitorFunc is a function that implements Visitor.
type VisitorFunc func(n *Node, raw json.RawMessage) error

// Visit implements Visitor.
func (f VisitorFunc) Visit(n *Node, raw json.RawMessage) error { return f(n, raw) }

// ---- Walk ------------------------------------------------------------------

// Walk traverses the ADF document in pre-order (parent before children),
// calling visitor.Visit for every node including the root doc node.
//
// doc may be nil or the JSON null value, in which case Walk returns nil
// immediately without calling the visitor.
//
// Walk does not validate the ADF schema beyond what is needed to traverse the
// tree; unknown node types are passed to the visitor as-is so callers can
// decide how to handle them.
func Walk(doc json.RawMessage, visitor Visitor) error {
	if len(doc) == 0 || string(doc) == "null" {
		return nil
	}

	var root Node
	if err := json.Unmarshal(doc, &root); err != nil {
		return errext.Errorf("adf: unmarshal root node: %w", err)
	}

	return walkNode(&root, doc, visitor)
}

// walkNode visits n and then recurses into its children.
// raw is the JSON bytes that produced n (used by the visitor for unknown nodes).
func walkNode(n *Node, raw json.RawMessage, visitor Visitor) error {
	if err := visitor.Visit(n, raw); err != nil {
		return err
	}

	if len(n.Content) == 0 {
		return nil
	}

	// Re-decode the content array as raw messages so each child gets its own
	// raw bytes for the visitor.
	var wrapper struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		// If we cannot get raw children, fall back to walking without raw.
		for i := range n.Content {
			if err2 := walkNode(&n.Content[i], nil, visitor); err2 != nil {
				return err2
			}
		}
		return nil
	}

	for i := range n.Content {
		var child Node
		if i < len(wrapper.Content) {
			if err := json.Unmarshal(wrapper.Content[i], &child); err != nil {
				return errext.Errorf("adf: unmarshal child node: %w", err)
			}
			if err := walkNode(&child, wrapper.Content[i], visitor); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- ExtractLinks ----------------------------------------------------------

// ExtractLinks traverses doc and returns every link mark found, in document
// order. Each link's URL is classified via classify.Classify using jiraSite as
// the configured Jira base URL.
//
// Deduplication is the caller's responsibility; this function may return the
// same URL more than once if it appears multiple times in the document.
//
// A null or empty doc returns a nil slice and no error.
func ExtractLinks(doc json.RawMessage, jiraSite string) ([]Link, error) {
	var links []Link

	err := Walk(doc, VisitorFunc(func(n *Node, _ json.RawMessage) error {
		if n.Type != "text" {
			return nil
		}
		for _, m := range n.Marks {
			if m.Type != "link" {
				continue
			}
			var la linkAttrs
			if len(m.Attrs) > 0 {
				_ = json.Unmarshal(m.Attrs, &la)
			}
			if la.Href == "" {
				continue
			}
			links = append(links, Link{
				URL:            la.Href,
				Text:           n.Text,
				Classification: classify.Classify(la.Href, jiraSite),
			})
		}
		return nil
	}))
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ---- RenderMarkdown --------------------------------------------------------

// RenderMarkdown converts an ADF document to Markdown.
//
// It returns:
//   - md: the rendered Markdown string.
//   - unknown: a slice of UnknownNode for every node type the renderer does
//     not handle. The renderer preserves the inner text of unknown nodes and
//     emits a Markdown comment so downstream readers can see something happened.
//   - err: non-nil only when the JSON cannot be decoded.
//
// A null or empty doc returns an empty string, a nil slice, and no error.
func RenderMarkdown(doc json.RawMessage) (string, []UnknownNode, error) {
	if len(doc) == 0 || string(doc) == "null" {
		return "", nil, nil
	}

	var root Node
	if err := json.Unmarshal(doc, &root); err != nil {
		return "", nil, errext.Errorf("adf: unmarshal document: %w", err)
	}

	r := &renderer{}
	r.renderNode(&root, doc, 0)
	return r.buf.String(), r.unknown, nil
}

// ---- renderer --------------------------------------------------------------

// renderer holds the mutable state accumulated while rendering a document.
type renderer struct {
	buf     strings.Builder
	unknown []UnknownNode
}

// knownNodeTypes is the set of node types the renderer handles explicitly.
var knownNodeTypes = map[string]bool{
	"doc":         true,
	"paragraph":   true,
	"heading":     true,
	"text":        true,
	"hardBreak":   true,
	"bulletList":  true,
	"orderedList": true,
	"listItem":    true,
	"codeBlock":   true,
	"blockquote":  true,
}

// knownMarkTypes is the set of mark types the renderer handles explicitly.
var knownMarkTypes = map[string]bool{
	"link":   true,
	"strong": true,
	"em":     true,
	"code":   true,
}

// renderNode dispatches to the appropriate render method based on node type.
// depth tracks list nesting for indentation.
func (r *renderer) renderNode(n *Node, raw json.RawMessage, depth int) {
	switch n.Type {
	case "doc":
		r.renderChildren(n, raw, depth)

	case "paragraph":
		r.renderParagraph(n, raw, depth)

	case "heading":
		r.renderHeading(n, raw, depth)

	case "text":
		r.renderText(n)

	case "hardBreak":
		r.buf.WriteString("  \n")

	case "bulletList":
		r.renderList(n, raw, depth, false)

	case "orderedList":
		r.renderList(n, raw, depth, true)

	case "listItem":
		// listItem is rendered by renderList; reaching here means a bare
		// listItem outside a list — render its children as a paragraph.
		r.renderChildren(n, raw, depth)

	case "codeBlock":
		r.renderCodeBlock(n, raw)

	case "blockquote":
		r.renderBlockquote(n, raw, depth)

	default:
		r.renderUnknown(n, raw)
	}
}

// renderChildren renders all children of n in order.
func (r *renderer) renderChildren(n *Node, raw json.RawMessage, depth int) {
	childRaws := extractChildRaws(raw)
	for i := range n.Content {
		var childRaw json.RawMessage
		if i < len(childRaws) {
			childRaw = childRaws[i]
		}
		r.renderNode(&n.Content[i], childRaw, depth)
	}
}

// renderParagraph renders a paragraph node, surrounding it with blank lines.
func (r *renderer) renderParagraph(n *Node, raw json.RawMessage, depth int) {
	r.ensureBlankLine()
	r.renderChildren(n, raw, depth)
	r.buf.WriteString("\n")
}

// renderHeading renders a heading node using ATX-style Markdown (#, ##, …).
func (r *renderer) renderHeading(n *Node, raw json.RawMessage, depth int) {
	var ha headingAttrs
	if len(n.Attrs) > 0 {
		_ = json.Unmarshal(n.Attrs, &ha)
	}
	level := ha.Level
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}

	r.ensureBlankLine()
	r.buf.WriteString(strings.Repeat("#", level))
	r.buf.WriteString(" ")
	r.renderChildren(n, raw, depth)
	r.buf.WriteString("\n")
}

// renderText renders a text node, applying its marks.
func (r *renderer) renderText(n *Node) {
	text := n.Text

	// Collect unknown marks for comment emission.
	var unknownMarks []mark
	for _, m := range n.Marks {
		if !knownMarkTypes[m.Type] {
			unknownMarks = append(unknownMarks, m)
		}
	}

	// Apply known marks in a fixed order so nesting is deterministic:
	// code wraps first (innermost), then em, then strong, then link (outermost).
	if hasMark(n.Marks, "code") {
		text = "`" + text + "`"
	}
	if hasMark(n.Marks, "em") {
		text = "*" + text + "*"
	}
	if hasMark(n.Marks, "strong") {
		text = "**" + text + "**"
	}
	if href := linkHref(n.Marks); href != "" {
		text = "[" + text + "](" + href + ")"
	}

	r.buf.WriteString(text)

	// Emit comments for unknown marks.
	for _, m := range unknownMarks {
		r.buf.WriteString(fmt.Sprintf("<!-- adf: unknown mark type %q -->", m.Type))
	}
}

// renderList renders a bulletList or orderedList node.
func (r *renderer) renderList(n *Node, raw json.RawMessage, depth int, ordered bool) {
	r.ensureBlankLine()
	childRaws := extractChildRaws(raw)
	for i := range n.Content {
		child := &n.Content[i]
		var childRaw json.RawMessage
		if i < len(childRaws) {
			childRaw = childRaws[i]
		}
		if child.Type != "listItem" {
			r.renderNode(child, childRaw, depth)
			continue
		}
		indent := strings.Repeat("  ", depth)
		if ordered {
			r.buf.WriteString(fmt.Sprintf("%s%d. ", indent, i+1))
		} else {
			r.buf.WriteString(indent + "- ")
		}
		// Render listItem children inline (paragraphs inside list items
		// should not add extra blank lines).
		r.renderListItemChildren(child, childRaw, depth+1)
		r.buf.WriteString("\n")
	}
}

// renderListItemChildren renders the children of a listItem without the
// paragraph blank-line wrapping, so list items stay compact.
func (r *renderer) renderListItemChildren(n *Node, raw json.RawMessage, depth int) {
	childRaws := extractChildRaws(raw)
	for i := range n.Content {
		child := &n.Content[i]
		var childRaw json.RawMessage
		if i < len(childRaws) {
			childRaw = childRaws[i]
		}
		// Paragraphs inside list items: render children directly (no blank lines).
		if child.Type == "paragraph" {
			r.renderChildren(child, childRaw, depth)
		} else {
			r.renderNode(child, childRaw, depth)
		}
	}
}

// renderCodeBlock renders a fenced code block.
func (r *renderer) renderCodeBlock(n *Node, raw json.RawMessage) {
	var ca codeBlockAttrs
	if len(n.Attrs) > 0 {
		_ = json.Unmarshal(n.Attrs, &ca)
	}
	r.ensureBlankLine()
	r.buf.WriteString("```")
	if ca.Language != "" {
		r.buf.WriteString(ca.Language)
	}
	r.buf.WriteString("\n")
	// Collect text content from children.
	for i := range n.Content {
		if n.Content[i].Type == "text" {
			r.buf.WriteString(n.Content[i].Text)
		}
	}
	r.buf.WriteString("\n```\n")
}

// renderBlockquote renders a blockquote node, prefixing each line with "> ".
func (r *renderer) renderBlockquote(n *Node, raw json.RawMessage, depth int) {
	// Render children into a temporary buffer, then prefix each line.
	sub := &renderer{}
	sub.renderChildren(n, raw, depth)
	content := sub.buf.String()
	r.unknown = append(r.unknown, sub.unknown...)

	r.ensureBlankLine()
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if line == "" {
			r.buf.WriteString(">\n")
		} else {
			r.buf.WriteString("> " + line + "\n")
		}
	}
}

// renderUnknown handles a node type the renderer does not recognise.
// It records the node in the unknown list, preserves any inner text, and
// emits a Markdown comment so downstream readers can see something happened.
func (r *renderer) renderUnknown(n *Node, raw json.RawMessage) {
	var rawToStore json.RawMessage
	if len(raw) > 0 {
		rawToStore = raw
	} else {
		// Best-effort re-encode if we lost the raw bytes.
		enc, _ := json.Marshal(n)
		rawToStore = enc
	}
	r.unknown = append(r.unknown, UnknownNode{NodeType: n.Type, Raw: rawToStore})
	r.buf.WriteString(fmt.Sprintf("<!-- adf: unknown node type %q -->", n.Type))
	// Preserve inner text content where present.
	text := collectText(n)
	if text != "" {
		r.buf.WriteString(text)
	}
}

// ---- helpers ---------------------------------------------------------------

// ensureBlankLine writes a blank line if the buffer does not already end with
// one. This keeps paragraph and heading spacing correct.
func (r *renderer) ensureBlankLine() {
	s := r.buf.String()
	if s == "" {
		return
	}
	if strings.HasSuffix(s, "\n\n") {
		return
	}
	if strings.HasSuffix(s, "\n") {
		r.buf.WriteString("\n")
		return
	}
	r.buf.WriteString("\n\n")
}

// extractChildRaws decodes the "content" array of a node's raw JSON into
// individual raw messages so each child can be passed to the visitor with its
// own bytes. Returns nil if raw is empty or has no content array.
func extractChildRaws(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var wrapper struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil
	}
	return wrapper.Content
}

// hasMark reports whether marks contains a mark of the given type.
func hasMark(marks []mark, typ string) bool {
	for _, m := range marks {
		if m.Type == typ {
			return true
		}
	}
	return false
}

// linkHref returns the href from the first link mark in marks, or "".
func linkHref(marks []mark) string {
	for _, m := range marks {
		if m.Type != "link" {
			continue
		}
		var la linkAttrs
		if len(m.Attrs) > 0 {
			_ = json.Unmarshal(m.Attrs, &la)
		}
		return la.Href
	}
	return ""
}

// collectText recursively gathers all text content from a node and its
// descendants. Used to preserve inner text of unknown nodes.
func collectText(n *Node) string {
	if n.Type == "text" {
		return n.Text
	}
	var sb strings.Builder
	for i := range n.Content {
		sb.WriteString(collectText(&n.Content[i]))
	}
	return sb.String()
}
