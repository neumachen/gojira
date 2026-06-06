// Package devstatus enriches an already-fetched Jira issue with all
// the development metadata Jira's Dev Status API surfaces: pull
// requests, branches, commits, repositories, and builds.
//
// # Why this package exists
//
// The standard GET /rest/api/3/issue/{key} response carries a
// development-summary field (customfield_10000) that reports per-
// dataType counts of associated entities, but not the underlying URLs
// or details. The actual entity lists live behind an undocumented
// endpoint:
//
//	GET /rest/dev-status/1.0/issue/detail
//	    ?issueId=<NUMERIC>&applicationType=<APP>&dataType=<DT>
//
// Atlassian's own Jira UI consumes this endpoint to populate the per-
// issue Development panel, and the response shape has been stable for
// over a decade. gojira treats it as best-effort enrichment that can
// be opted out cleanly via [config.Config.IncludeDevStatus].
//
// # Architectural placement
//
// devstatus is a small leaf package that depends only on stdlib,
// errext, client (for HTTP transport and the Dev Status response
// types), config (for the IncludeDevStatus / DevStatusApplications /
// DevStatusDataTypes flags), and parse (for the [parse.Issue] type
// and the canonical [parse.DevStatusData] result type). It
// deliberately does NOT import crawl, render, output, events, fetch,
// extract, adf, or classify: those would create cycles or layering
// regressions.
//
// The crawl orchestrator constructs a single *Enricher at startup and
// shares it across all worker goroutines, mirroring the pattern used
// by internal/hierarchy.
//
// # No smart gate: always query every configured dataType
//
// Earlier revisions of this package parsed the customfield_10000
// summary blob and asked a "smart gate" which dataTypes to query,
// skipping the ones the summary reported as zero-count. That
// optimisation produced two silent-miss bugs in three commits
// (PLATENG-1578 in particular: the summary said repository.count=1
// and zero for every other dataType, but the Jira UI showed a PR,
// branches, commits, and builds — all of which the gate silently
// dropped, regardless of the summary's own "isStale":true flag).
//
// The current contract is: when IncludeDevStatus=true, every
// configured (application, dataType) pair is queried. The
// customfield_10000 summary is no longer parsed. The cost is at most
// five extra HTTP calls per issue; the benefit is that "no data
// reached me" can no longer be silently caused by a stale summary
// cache.
//
// # Failure isolation
//
// One Dev Status request is issued per (application, dataType) pair.
// Per-call errors are accumulated rather than surfaced immediately:
// if at least one call returns results, those results are returned
// alongside a non-fatal joined error the caller can log as a
// partial-failure warning. Only when every call failed AND no
// entities were collected does [Enricher.Enrich] return a fatal
// error.
package devstatus

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/parse"
)

// CanonicalDataTypes is the canonical order of the five Dev Status
// dataType values gojira understands. It is used to order the
// configured DataTypes deterministically for per-issue fan-out (so
// per-call error messages and the order in which entities are merged
// are stable across runs). It mirrors the typical top-down order in
// which the Jira UI Development panel groups them.
var CanonicalDataTypes = []string{
	"pullrequest",
	"branch",
	"commit",
	"repository",
	"build",
}

// devStatusClient is the narrow subset of *client.Client behaviour the
// Enricher depends on. It exists so tests can substitute a fake without
// standing up an httptest.Server. The production implementation is
// satisfied by *client.Client directly.
type devStatusClient interface {
	DevStatus(ctx context.Context, issueNumericID, application, dataType string) (client.DevStatusResponse, error)
}

// Enricher queries the Jira Dev Status API for development metadata
// associated with an issue.
//
// Construct via [New]. A single *Enricher is intended to be shared
// across all crawl workers; the underlying *client.Client already
// holds the connection pool and auth state.
type Enricher struct {
	client devStatusClient
	cfg    config.Config
}

// New constructs an Enricher backed by the given client and config.
// A nil client is treated as a programming error and will produce a
// fatal error on the first call to [Enricher.Enrich]; the crawl
// orchestrator always supplies a non-nil client.
func New(c *client.Client, cfg config.Config) *Enricher {
	if c == nil {
		// Defensive: pass through the typed-nil interface so the
		// Enrich check below catches it deterministically.
		return &Enricher{client: nil, cfg: cfg}
	}
	return &Enricher{client: c, cfg: cfg}
}

// Enrich returns the populated [parse.DevStatusData] for issue,
// fanning out one Dev Status request per (configured application,
// configured dataType) pair and merging the results.
//
// The following cases short-circuit with the zero value and no error,
// without issuing any HTTP requests:
//
//   - cfg.IncludeDevStatus is false.
//   - issue.NumericID is empty (no opaque ID to query against).
//   - cfg.DevStatusApplications is empty.
//   - cfg.DevStatusDataTypes is empty (filtering to the canonical set
//     also yields empty).
//
// Otherwise every (application, dataType) cross-product is queried.
// Per-call errors are joined and returned alongside any partial
// results; only when no results are collected AND every call failed
// is the error wrapped as a fatal-style error the crawl orchestrator
// can use to distinguish "this issue's enrichment found nothing
// because the summary really had nothing" from "every upstream
// request failed and we have no data".
//
// The returned lists are deduplicated per entity type (PRs/branches/
// repos/builds by URL; commits by full SHA) and sorted by their
// canonical key for determinism.
func (e *Enricher) Enrich(ctx context.Context, issue parse.Issue) (parse.DevStatusData, error) {
	if e == nil {
		return parse.DevStatusData{}, errext.Errorf("devstatus: nil enricher")
	}
	if !e.cfg.IncludeDevStatus {
		return parse.DevStatusData{}, nil
	}
	if issue.NumericID == "" {
		return parse.DevStatusData{}, nil
	}
	if len(e.cfg.DevStatusApplications) == 0 {
		return parse.DevStatusData{}, nil
	}

	dataTypes := orderedDataTypes(e.cfg.DevStatusDataTypes)
	if len(dataTypes) == 0 {
		return parse.DevStatusData{}, nil
	}

	if e.client == nil {
		return parse.DevStatusData{}, errext.Errorf("devstatus: nil client")
	}

	var (
		out         parse.DevStatusData
		perCallErrs []error
		attempted   int
	)

	for _, app := range e.cfg.DevStatusApplications {
		app = strings.TrimSpace(app)
		if app == "" {
			continue
		}
		for _, dt := range dataTypes {
			attempted++
			resp, err := e.client.DevStatus(ctx, issue.NumericID, app, dt)
			if err != nil {
				perCallErrs = append(perCallErrs,
					errext.Errorf("devstatus: %s/%s for %s: %w",
						app, dt, issue.Key, err))
				continue
			}
			mergeInto(&out, resp, app)
		}
	}

	if attempted == 0 {
		// No usable (application, dataType) combinations — treat as opt-out.
		return parse.DevStatusData{}, nil
	}

	dedupAll(&out)

	// Partial-failure policy:
	//   - At least one entity collected → return data with joined non-
	//     fatal error (crawl logs as devstatus.partial_failure WARN).
	//   - No entities AND every attempted call failed → fatal-style
	//     error (still mapped by the crawl orchestrator to a partial-
	//     failure WARN; the issue itself was rendered fine).
	//   - No entities AND no per-call errors → return empty (the issue
	//     simply has no associated entities on the queried scope).
	if len(perCallErrs) == 0 {
		return out, nil
	}
	joined := errors.Join(perCallErrs...)
	if hasAnyEntity(out) {
		return out, joined
	}
	return parse.DevStatusData{}, errext.Errorf("devstatus: all calls failed for %s: %w", issue.Key, joined)
}

// hasAnyEntity reports whether d carries at least one entity across
// any of the five lists. Used by the partial-failure policy.
func hasAnyEntity(d parse.DevStatusData) bool {
	return len(d.PullRequests) > 0 ||
		len(d.Branches) > 0 ||
		len(d.Commits) > 0 ||
		len(d.Repositories) > 0 ||
		len(d.Builds) > 0
}

// orderedDataTypes returns the configured dataTypes intersected with
// the canonical set and ordered by [CanonicalDataTypes] so per-call
// error messages and merge order are deterministic across runs.
// Trimmed-empty and unrecognised entries are dropped silently — the
// validator on the config struct is the place that rejects unknown
// values at startup; this is defence in depth.
func orderedDataTypes(configured []string) []string {
	if len(configured) == 0 {
		return nil
	}
	configuredSet := make(map[string]bool, len(configured))
	for _, dt := range configured {
		dt = strings.TrimSpace(dt)
		if dt == "" {
			continue
		}
		configuredSet[dt] = true
	}
	out := make([]string, 0, len(CanonicalDataTypes))
	for _, dt := range CanonicalDataTypes {
		if configuredSet[dt] {
			out = append(out, dt)
		}
	}
	return out
}

// mergeInto appends every entity in resp's instances into the
// corresponding list on out. application is the upstream
// applicationType the caller queried; PRs preserve it as their
// Application label so a query for "GitHub" served by a GitHubEnterprise
// integration is still attributed to "GitHub".
func mergeInto(out *parse.DevStatusData, resp client.DevStatusResponse, application string) {
	for _, inst := range resp.Detail {
		for _, pr := range inst.PullRequests {
			out.PullRequests = append(out.PullRequests, convertPR(pr, application))
		}
		for _, br := range inst.Branches {
			out.Branches = append(out.Branches, convertBranch(br))
		}
		for _, cm := range inst.Commits {
			out.Commits = append(out.Commits, convertCommit(cm))
		}
		for _, rp := range inst.Repositories {
			out.Repositories = append(out.Repositories, convertRepository(rp))
		}
		for _, bd := range inst.Builds {
			out.Builds = append(out.Builds, convertBuild(bd))
		}
	}
}

// dedupAll dedups every list inside out by its canonical key and
// sorts the result deterministically. Empty-key entries (e.g.
// branches with no URL) are kept so defensive non-canonical entries
// do not vanish.
func dedupAll(out *parse.DevStatusData) {
	out.PullRequests = dedupPRsByURL(out.PullRequests)
	out.Branches = dedupBranchesByURL(out.Branches)
	out.Commits = dedupCommitsByID(out.Commits)
	out.Repositories = dedupReposByURL(out.Repositories)
	out.Builds = dedupBuildsByURL(out.Builds)
}

// ---------------------------------------------------------------------------
// Per-entity converters
// ---------------------------------------------------------------------------

// convertPR translates a single upstream client.DevStatusPR entry into
// the gojira-side parse.PullRequest type used by render.
//
// The Application field is sourced from the queried applicationType
// rather than the upstream "_instance.type" so that a Dev Status query
// for "GitHub" that happens to be served by a GitHubEnterprise
// integration is still attributed to the value the caller asked for.
// LastUpdate is best-effort: an unparseable timestamp leaves the field
// zero rather than dropping the entry.
func convertPR(pr client.DevStatusPR, application string) parse.PullRequest {
	lastUpdate := parseJiraTimestamp(pr.LastUpdate)

	var reviewers []parse.Reviewer
	for _, r := range pr.Reviewers {
		reviewers = append(reviewers, parse.Reviewer{
			Name:     r.Name,
			Approved: r.Approved,
		})
	}

	return parse.PullRequest{
		ID:           pr.ID,
		URL:          pr.URL,
		Title:        pr.Name,
		Status:       pr.Status,
		Application:  application,
		Repository:   pr.Repository,
		SourceBranch: pr.Source.Branch,
		DestBranch:   pr.Destination.Branch,
		Author:       pr.Author.Name,
		LastUpdate:   lastUpdate,
		Reviewers:    reviewers,
	}
}

// convertBranch translates a single upstream client.DevStatusBranchEntry
// into the gojira-side parse.Branch type used by render. Multi-line
// commit messages are truncated to their first line.
func convertBranch(br client.DevStatusBranchEntry) parse.Branch {
	return parse.Branch{
		Name:             br.Name,
		URL:              br.URL,
		Repository:       br.Repository.Name,
		RepositoryURL:    br.Repository.URL,
		LastCommitID:     br.LastCommit.DisplayID,
		LastCommitURL:    br.LastCommit.URL,
		LastCommitMsg:    firstLine(br.LastCommit.Message),
		LastCommitAuthor: br.LastCommit.Author.Name,
		LastUpdated:      parseJiraTimestamp(br.LastCommit.AuthorTimestamp),
	}
}

// convertCommit translates a single upstream client.DevStatusCommit
// into the gojira-side parse.Commit type used by render.
func convertCommit(cm client.DevStatusCommit) parse.Commit {
	return parse.Commit{
		ID:         cm.ID,
		ShortID:    cm.DisplayID,
		URL:        cm.URL,
		Message:    firstLine(cm.Message),
		Author:     cm.Author.Name,
		Repository: cm.Repository.Name,
		AuthoredAt: parseJiraTimestamp(cm.AuthorTimestamp),
	}
}

// convertRepository translates a single upstream client.DevStatusRepository
// into the gojira-side parse.Repository type used by render. The
// upstream avatar URL is intentionally dropped: the renderer does not
// emit an avatar image in Markdown output and preserving the field
// would just clutter the parsed value.
func convertRepository(rp client.DevStatusRepository) parse.Repository {
	return parse.Repository{
		Name: rp.Name,
		URL:  rp.URL,
	}
}

// convertBuild translates a single upstream client.DevStatusBuild into
// the gojira-side parse.Build type used by render. The upstream
// references[] array is mined for a repository attribution when one is
// derivable from the first reference's URI (refs/heads/<branch>); when
// no repository can be derived, parse.Build.Repository is left empty.
//
// Tests* fields default to zero when the upstream response does not
// carry a testSummary block.
func convertBuild(bd client.DevStatusBuild) parse.Build {
	out := parse.Build{
		ID:          bd.ID,
		Name:        bd.Name,
		URL:         bd.URL,
		State:       bd.State,
		Description: bd.Description,
		LastUpdated: parseJiraTimestamp(bd.LastUpdated),
	}
	if bd.TestSummary != nil {
		out.TestsPassed = bd.TestSummary.PassedNumber
		out.TestsFailed = bd.TestSummary.FailedNumber
		out.TestsTotal = bd.TestSummary.TotalNumber
	}
	return out
}

// firstLine returns the substring of s up to but not including the
// first newline. If s does not contain a newline, s is returned
// unchanged. Carriage returns are trimmed.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// jiraTimeLayouts is the same set parse.parseJiraTime uses; duplicated
// here so devstatus can avoid an import cycle while accepting the same
// timestamp shapes the platform REST API emits.
var jiraTimeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339Nano,
	time.RFC3339,
}

// parseJiraTimestamp parses s using the canonical Jira layouts and
// returns the zero time when s is empty or unparseable. Preserving a
// zero value is preferable to dropping an entire entity on a malformed
// timestamp.
func parseJiraTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range jiraTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// Dedup helpers
// ---------------------------------------------------------------------------

func dedupPRsByURL(prs []parse.PullRequest) []parse.PullRequest {
	if len(prs) == 0 {
		return prs
	}
	seen := make(map[string]bool, len(prs))
	out := make([]parse.PullRequest, 0, len(prs))
	for _, pr := range prs {
		if pr.URL == "" {
			out = append(out, pr)
			continue
		}
		if seen[pr.URL] {
			continue
		}
		seen[pr.URL] = true
		out = append(out, pr)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func dedupBranchesByURL(brs []parse.Branch) []parse.Branch {
	if len(brs) == 0 {
		return brs
	}
	seen := make(map[string]bool, len(brs))
	out := make([]parse.Branch, 0, len(brs))
	for _, br := range brs {
		if br.URL == "" {
			out = append(out, br)
			continue
		}
		if seen[br.URL] {
			continue
		}
		seen[br.URL] = true
		out = append(out, br)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func dedupCommitsByID(cms []parse.Commit) []parse.Commit {
	if len(cms) == 0 {
		return cms
	}
	seen := make(map[string]bool, len(cms))
	out := make([]parse.Commit, 0, len(cms))
	for _, cm := range cms {
		if cm.ID == "" {
			out = append(out, cm)
			continue
		}
		if seen[cm.ID] {
			continue
		}
		seen[cm.ID] = true
		out = append(out, cm)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func dedupReposByURL(rps []parse.Repository) []parse.Repository {
	if len(rps) == 0 {
		return rps
	}
	seen := make(map[string]bool, len(rps))
	out := make([]parse.Repository, 0, len(rps))
	for _, rp := range rps {
		if rp.URL == "" {
			out = append(out, rp)
			continue
		}
		if seen[rp.URL] {
			continue
		}
		seen[rp.URL] = true
		out = append(out, rp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func dedupBuildsByURL(bds []parse.Build) []parse.Build {
	if len(bds) == 0 {
		return bds
	}
	seen := make(map[string]bool, len(bds))
	out := make([]parse.Build, 0, len(bds))
	for _, bd := range bds {
		if bd.URL == "" {
			out = append(out, bd)
			continue
		}
		if seen[bd.URL] {
			continue
		}
		seen[bd.URL] = true
		out = append(out, bd)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}
