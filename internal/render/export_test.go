package render

import "encoding/json"

// ClassifyCustomFieldForTest exposes the unexported
// classifyCustomField helper to the package-external test file
// (render_test package) so the four-kind classification can be
// exercised in isolation from the surrounding RenderIssue plumbing.
// Production code must continue to call classifyCustomField directly.
func ClassifyCustomFieldForTest(raw json.RawMessage) (kind string, pretty string, indented bool) {
	return classifyCustomField(raw)
}

// PrettifyAtlassianBlobForTest exposes the unexported
// prettifyAtlassianBlob walker to the package-external test file
// (render_test package) so the Map.toString()+JSON walker can be
// exercised directly, in isolation from the kindStringStructured
// rendering branch that calls it. The same export-hook pattern as
// ClassifyCustomFieldForTest applies: production code must continue
// to call prettifyAtlassianBlob.
func PrettifyAtlassianBlobForTest(s string) (pretty string, ok bool) {
	return prettifyAtlassianBlob(s)
}
