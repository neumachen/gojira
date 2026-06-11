package client

import (
	"encoding/json"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/adf"
)

// fieldsBuilder assembles the "fields" and "update" objects of a Jira v3
// create/update request body. It is the single place where issue field
// marshalling lives, so the CreateIssue/UpdateIssue/TransitionIssue methods
// (added in later Phase 2 tasks) stay thin: they seed required values,
// apply caller options, and emit.
//
// Adding support for a new Jira field is a new With* option on this file —
// or, with no code change at all, via [WithField] / [WithRawFields]. The
// method signatures never widen.
type fieldsBuilder struct {
	fields map[string]any
	update map[string]any
}

func newFieldsBuilder() *fieldsBuilder {
	return &fieldsBuilder{
		fields: map[string]any{},
		update: map[string]any{},
	}
}

// setField sets fields[id] = value.
func (b *fieldsBuilder) setField(id string, value any) {
	b.fields[id] = value
}

// mergeFields merges a caller-supplied map into the fields object. Later
// keys overwrite earlier ones; existing keys set by typed options are
// overwritten by raw entries with the same id, which matches the
// "WithRawFields wins" intent of the escape hatch.
func (b *fieldsBuilder) mergeFields(m map[string]any) {
	for k, v := range m {
		b.fields[k] = v
	}
}

// addUpdate appends an update op (e.g. {"add": value}) under update[id].
// The slice ordering is the caller's order — Jira applies update ops in
// list order, which matters for add+remove sequences.
func (b *fieldsBuilder) addUpdate(id, verb string, value any) {
	ops, _ := b.update[id].([]any)
	b.update[id] = append(ops, map[string]any{verb: value})
}

// body returns the request body map, omitting "fields" / "update" when
// empty so we never send Jira a {"fields":{}} blob (which would be a
// no-op edit but also draws warnings in some Jira proxies).
func (b *fieldsBuilder) body() map[string]any {
	out := map[string]any{}
	if len(b.fields) > 0 {
		out["fields"] = b.fields
	}
	if len(b.update) > 0 {
		out["update"] = b.update
	}
	return out
}

// marshal returns the JSON request body bytes.
func (b *fieldsBuilder) marshal() ([]byte, error) {
	bz, err := json.Marshal(b.body())
	if err != nil {
		return nil, errext.Errorf("client: marshal request body: %w", err)
	}
	return bz, nil
}

// ---- Option types -----------------------------------------------------------

// CreateOption customizes a CreateIssue request body. It is shaped as a
// function over the internal builder so each option stays a tiny,
// independently-testable unit and new options can be added with zero
// disruption to existing call sites.
type CreateOption func(*fieldsBuilder)

// UpdateOption customizes an UpdateIssue request body. Same shape as
// [CreateOption] but a distinct named type so the compiler keeps
// Create-only options (like [WithParent]) out of Update call sites.
type UpdateOption func(*fieldsBuilder)

// fieldOp is the unexported common form. Every typed option below is a
// thin wrapper that delegates to one of these so the marshalling logic
// for a given field lives in EXACTLY ONE place — no copy-paste drift
// between Create and Update variants.
type fieldOp func(*fieldsBuilder)

// ---- Shared field ops -------------------------------------------------------
//
// Each opXxx is the single source of truth for how a field is written
// into the body. Create-typed and Update-typed wrappers below convert
// the shared op into the appropriate named type. Adding a new field is
// (1) a new opXxx here, (2) one WithXxx and one WithXxxUpdate wrapper.

func opSummary(s string) fieldOp {
	return func(b *fieldsBuilder) { b.setField("summary", s) }
}

func opDescriptionText(text string) fieldOp {
	// Store the ADF doc as json.RawMessage so it marshals as an
	// embedded JSON object, NOT a quoted string.
	doc := adf.BuildParagraphDoc(text)
	return func(b *fieldsBuilder) { b.setField("description", doc) }
}

func opDescriptionADF(doc json.RawMessage) fieldOp {
	return func(b *fieldsBuilder) { b.setField("description", doc) }
}

func opAssigneeAccountID(id string) fieldOp {
	return func(b *fieldsBuilder) {
		b.setField("assignee", map[string]any{"accountId": id})
	}
}

func opLabels(labels ...string) fieldOp {
	// Copy to decouple from the caller's slice (cheap, defensive).
	cp := append([]string(nil), labels...)
	return func(b *fieldsBuilder) { b.setField("labels", cp) }
}

func opParent(key string) fieldOp {
	return func(b *fieldsBuilder) {
		b.setField("parent", map[string]any{"key": key})
	}
}

func opField(fieldID string, value any) fieldOp {
	return func(b *fieldsBuilder) { b.setField(fieldID, value) }
}

func opRawFields(m map[string]any) fieldOp {
	return func(b *fieldsBuilder) { b.mergeFields(m) }
}

// ---- Create-typed options ---------------------------------------------------

// WithSummary sets the issue summary on a Create request.
func WithSummary(s string) CreateOption { return CreateOption(opSummary(s)) }

// WithDescriptionText sets the issue description from plain text on a
// Create request. The text is converted to ADF via
// [adf.BuildParagraphDoc] so Jira accepts it.
func WithDescriptionText(text string) CreateOption {
	return CreateOption(opDescriptionText(text))
}

// WithDescriptionADF sets the issue description from a caller-supplied
// ADF document on a Create request. Use this when you already have rich
// ADF (lists, code blocks, etc.) you do not want to re-parse from text.
func WithDescriptionADF(doc json.RawMessage) CreateOption {
	return CreateOption(opDescriptionADF(doc))
}

// WithAssigneeAccountID sets the assignee on a Create request by
// Atlassian accountId — the only assignee write form Jira Cloud
// supports since the 2019 GDPR migration.
func WithAssigneeAccountID(id string) CreateOption {
	return CreateOption(opAssigneeAccountID(id))
}

// WithLabels sets the labels array on a Create request.
func WithLabels(labels ...string) CreateOption { return CreateOption(opLabels(labels...)) }

// WithParent sets the parent issue key on a Create request (e.g. a
// Sub-task's parent). Update has its own "parent" semantics that differ
// per workflow; we therefore expose WithParent only on Create.
func WithParent(key string) CreateOption { return CreateOption(opParent(key)) }

// WithField is the generic escape hatch: set fields[fieldID]=value with
// no typed wrapper. Use this for any custom field or for fields the
// typed options above do not cover. This is the seam that lets gojira
// support a new Jira field with ZERO signature churn.
func WithField(fieldID string, value any) CreateOption {
	return CreateOption(opField(fieldID, value))
}

// WithRawFields merges a caller-supplied map of fieldID→value into the
// fields object. Useful when the gRPC layer hands in a bulk map of
// custom-field overrides. Conflicts with earlier typed options resolve
// last-write-wins.
func WithRawFields(m map[string]any) CreateOption {
	return CreateOption(opRawFields(m))
}

// ---- Update-typed options ---------------------------------------------------

// WithSummaryUpdate sets the issue summary on an Update request.
func WithSummaryUpdate(s string) UpdateOption { return UpdateOption(opSummary(s)) }

// WithDescriptionTextUpdate sets the issue description from plain text
// on an Update request, converting to ADF via [adf.BuildParagraphDoc].
func WithDescriptionTextUpdate(text string) UpdateOption {
	return UpdateOption(opDescriptionText(text))
}

// WithDescriptionADFUpdate sets the issue description from a
// caller-supplied ADF document on an Update request.
func WithDescriptionADFUpdate(doc json.RawMessage) UpdateOption {
	return UpdateOption(opDescriptionADF(doc))
}

// WithAssigneeAccountIDUpdate sets the assignee by accountId on an
// Update request.
func WithAssigneeAccountIDUpdate(id string) UpdateOption {
	return UpdateOption(opAssigneeAccountID(id))
}

// WithLabelsUpdate sets the labels array on an Update request. Use
// [WithUpdateVerb] for incremental "add" / "remove" semantics.
func WithLabelsUpdate(labels ...string) UpdateOption {
	return UpdateOption(opLabels(labels...))
}

// WithFieldUpdate is the generic escape hatch on Update: set
// fields[fieldID]=value with no typed wrapper.
func WithFieldUpdate(fieldID string, value any) UpdateOption {
	return UpdateOption(opField(fieldID, value))
}

// WithRawFieldsUpdate merges a caller-supplied map into the fields
// object on an Update request.
func WithRawFieldsUpdate(m map[string]any) UpdateOption {
	return UpdateOption(opRawFields(m))
}

// WithUpdateVerb appends an update op (verb is "add", "remove", or
// "set") for fieldID. Multi-valued fields like "labels" support
// incremental edits — call WithUpdateVerb several times to compose an
// operation list, applied by Jira in order. Update-only because the
// update verbs only have a defined meaning on existing issues.
func WithUpdateVerb(fieldID, verb string, value any) UpdateOption {
	return func(b *fieldsBuilder) { b.addUpdate(fieldID, verb, value) }
}

// ---- Public entry points (Render*Body) --------------------------------------

// RenderCreateBody assembles the JSON body for [Client.CreateIssue] from
// the required project key + issuetype name plus zero or more options.
// It is exported so the gojira facade can offer a dry-run affordance
// (build-without-send) reusing exactly the same assembly path the real
// CreateIssue method will use.
func RenderCreateBody(project, issueType string, opts ...CreateOption) ([]byte, error) {
	b := newFieldsBuilder()
	// Required values seeded BEFORE caller options so a caller could in
	// principle override them via WithField — that is intentional: it
	// preserves the "any field via WithField" escape-hatch property.
	b.setField("project", map[string]any{"key": project})
	b.setField("issuetype", map[string]any{"name": issueType})
	for _, opt := range opts {
		opt(b)
	}
	return b.marshal()
}

// RenderUpdateBody assembles the JSON body for [Client.UpdateIssue] from
// zero or more options. With no options the result is "{}" — a no-op
// edit that Jira accepts without complaint, rather than the malformed
// {"fields":{}} that some servers reject.
func RenderUpdateBody(opts ...UpdateOption) ([]byte, error) {
	b := newFieldsBuilder()
	for _, opt := range opts {
		opt(b)
	}
	return b.marshal()
}
