// Package output is the filesystem writer for gojira.
// This file defines the Store interface, which decouples the crawl
// orchestrator from the concrete filesystem implementation.
package output

import "context"

// Store is the injectable output destination for a crawl run.
// Implementations are called exactly once per Jira issue and must not
// retain the content after Write returns.
//
// Write persists the rendered Markdown for a single issue:
//   - key:        Jira issue key (e.g. "PLATENG-1147").
//   - indexMD:    content for <key>/index.md.
//   - outboundMD: content for <key>/references/outbound.md.
//     An empty string means no outbound file is written, but
//     implementations should still create the references/ directory.
//
// Write returns [ErrAlreadyExists] when the destination already exists
// and the implementation is configured to skip existing issues.
// Callers can test for this with errors.Is.
type Store interface {
	Write(ctx context.Context, key, indexMD, outboundMD string) error
}
