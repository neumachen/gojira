// Package hierarchy discovers Jira hierarchy children for an already-fetched
// issue via JQL search.
//
// # Why this package exists
//
// The Jira Cloud REST API v3 GET /rest/api/3/issue/{key} response includes
// fields.subtasks (the legacy Sub-task type only) but NOT children of the
// modern parent-link hierarchy. Children of an Epic, Story, Task, or Bug
// are discovered by issuing the inverse JQL query:
//
//	parent = "KEY"
//
// In tenants that still use the legacy Epic Link custom field (instead of
// the modern parent field), the equivalent query is:
//
//	"Epic Link" = "KEY"
//
// hierarchy.Discoverer runs both queries (when applicable) and returns the
// deduplicated union of child keys.
//
// # Architectural placement
//
// hierarchy is a small leaf package that depends only on stdlib, errext,
// client (for HTTP), config (for limits and field overrides), and parse
// (for the Issue type passed to Children). It deliberately does NOT import
// crawl, render, output, events, fetch, extract, adf, or classify: those
// would create cycles or layering regressions. The crawl orchestrator
// constructs a single *Discoverer at startup, holds it on its crawler
// struct, and calls Children() from each worker after the per-issue
// fetch+parse+extract+render+output sequence completes.
//
// # Auto-detection caching
//
// The Epic Link custom field ID varies per tenant. Discoverer auto-detects
// it from the tenant field metadata on first use (via sync.Once) and caches
// the result so subsequent calls do not re-query /rest/api/3/field. When
// cfg.EpicLinkField is non-empty, the configured override is used directly
// and the metadata endpoint is never called.
//
// # Failure isolation
//
// If the parent search succeeds and the Epic Link search fails, Children
// returns the parent results alongside a non-fatal wrapped error the caller
// can log but ignore. Only when both queries fail (or the only-run query
// fails) does Children return a nil/empty slice with a fatal error.
package hierarchy

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/parse"
)

// Discoverer queries Jira for hierarchy children of a given issue.
//
// Construct via [New]. A single *Discoverer is intended to be shared across
// all crawl workers; the Epic Link auto-detection result is cached via
// sync.Once so repeated calls do not re-query the tenant metadata endpoint.
type Discoverer struct {
	client *client.Client
	cfg    config.Config

	// once guards the lazy Epic Link auto-detection.
	once sync.Once
	// epicLinkID holds the resolved Epic Link custom field ID. Empty when
	// no Epic Link field exists on the tenant (or when auto-detection
	// hit a sticky error).
	epicLinkID string
	// epicLinkErr captures any error encountered during auto-detection.
	// It is non-fatal: the parent search still runs.
	epicLinkErr error
}

// New constructs a Discoverer for the given client and validated config.
// A nil client is treated as a programming error and will panic on first
// use; the crawl orchestrator always supplies a non-nil client.
func New(c *client.Client, cfg config.Config) *Discoverer {
	return &Discoverer{client: c, cfg: cfg}
}

// HierarchyCapable reports whether the given Jira issue type can have
// hierarchy children worth searching for.
//
// The only excluded category is the legacy "Sub-task" type, because sub-
// tasks are themselves children of a parent issue and (per Jira's data
// model) cannot have their own hierarchical children.
//
// For every other type — Epic, Story, Task, Bug, Improvement, New Feature,
// and any custom issue type — HierarchyCapable returns true. For unknown
// or empty types it also defaults to true: the cost of one extra
// per-issue JQL search is small, while the cost of silently missing
// children for an unrecognised type is high.
func HierarchyCapable(issueType string) bool {
	switch strings.ToLower(strings.TrimSpace(issueType)) {
	case "sub-task", "subtask":
		return false
	default:
		return true
	}
}

// Children returns the deduplicated, alphabetically-sorted set of
// hierarchy child keys for issue.
//
// It always runs the modern `parent = "KEY"` JQL query. When the Epic
// Link custom field is configured (cfg.EpicLinkField non-empty) or
// auto-detected from the tenant field metadata, it additionally runs
// `"Epic Link" = "KEY"` and merges the result. The two result sets are
// deduplicated (a child that appears in both queries is returned once).
//
// Up to cfg.ChildSearchLimit keys are requested per query; the returned
// slice is bounded by this limit across the merged result.
//
// # Error semantics
//
// If only the Epic Link query fails (and the parent query succeeds, or
// vice versa), Children returns the successful query's results plus a
// non-fatal wrapped error the caller can log but ignore. If every query
// that was attempted fails, Children returns a nil/empty slice with a
// fatal wrapped error.
func (d *Discoverer) Children(ctx context.Context, issue parse.Issue) ([]string, error) {
	if d == nil || d.client == nil {
		return nil, errext.Errorf("hierarchy: nil client")
	}
	if issue.Key == "" {
		return nil, errext.Errorf("hierarchy: empty issue key")
	}

	limit := d.cfg.ChildSearchLimit
	if limit <= 0 {
		limit = 100
	}

	seen := make(map[string]struct{})
	add := func(keys []string) {
		for _, k := range keys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if _, ok := seen[k]; ok {
				continue
			}
			if len(seen) >= limit {
				return
			}
			seen[k] = struct{}{}
		}
	}

	var firstErr, secondErr error
	queriesRun := 0

	// 1) Modern parent hierarchy.
	queriesRun++
	parentJQL := `parent = "` + issue.Key + `"`
	parentRes, parentErr := d.client.Search(ctx, parentJQL, limit)
	if parentErr != nil {
		firstErr = errext.Errorf("hierarchy: parent search for %s: %w", issue.Key, parentErr)
	} else {
		add(parentRes.Keys)
	}

	// 2) Epic Link (legacy). Only run when we have a field ID, configured
	//    or auto-detected.
	epicID := d.resolveEpicLinkField(ctx)
	if epicID != "" {
		queriesRun++
		epicJQL := `"Epic Link" = "` + issue.Key + `"`
		epicRes, epicErr := d.client.Search(ctx, epicJQL, limit)
		if epicErr != nil {
			secondErr = errext.Errorf("hierarchy: epic-link search for %s: %w", issue.Key, epicErr)
		} else {
			add(epicRes.Keys)
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)

	// Error policy:
	//  - both queries that ran failed → fatal.
	//  - only one of two queries failed → non-fatal: return results + wrapped err.
	//  - the only query that ran failed → fatal.
	switch {
	case firstErr != nil && secondErr != nil:
		return out, errext.Errorf("hierarchy: all child queries failed for %s: %w", issue.Key, firstErr)
	case firstErr != nil && queriesRun == 1:
		return out, firstErr
	case firstErr != nil:
		// parent failed, epic succeeded → partial success.
		return out, firstErr
	case secondErr != nil:
		// epic failed, parent succeeded → partial success.
		return out, secondErr
	default:
		return out, nil
	}
}

// resolveEpicLinkField returns the Epic Link custom field ID for the
// tenant, either from the configured override or from a cached
// auto-detection lookup. Returns an empty string when no Epic Link
// field exists on the tenant or when auto-detection has failed.
//
// The lookup is performed at most once per Discoverer via sync.Once.
// Failures are sticky: a transport error during auto-detection is
// remembered in d.epicLinkErr but the configured-override path is
// unaffected.
func (d *Discoverer) resolveEpicLinkField(ctx context.Context) string {
	// Configured override wins; no remote lookup needed.
	if strings.TrimSpace(d.cfg.EpicLinkField) != "" {
		return strings.TrimSpace(d.cfg.EpicLinkField)
	}

	d.once.Do(func() {
		fields, err := d.client.ListFields(ctx)
		if err != nil {
			d.epicLinkErr = errext.Errorf("hierarchy: list fields: %w", err)
			return
		}
		for _, f := range fields {
			if strings.EqualFold(strings.TrimSpace(f.Name), "Epic Link") {
				d.epicLinkID = f.ID
				return
			}
		}
		// No Epic Link field on this tenant — cached as empty.
	})

	return d.epicLinkID
}
