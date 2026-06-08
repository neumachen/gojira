// Command gojira-client is a tiny reference client for the gojira gRPC
// server, intended as a smoke / interoperability tool, not a production
// frontend.
//
// It exercises the gojira.v1 service surface:
//
//   - Classify         — pass any URL or bare issue key via -classify.
//   - GetIssue         — pass an issue key via -key (with -format=markdown|json|structured).
//   - Crawl            — pass one or more start keys as positional args, or via -crawl.
//   - CreateIssue      — -create-project/-create-type/-create-summary (with -dry-run).
//   - AddComment       — -comment with -comment-text.
//   - ListTransitions  — -transitions <KEY>.
//   - TransitionIssue  — -transition <KEY> with either -transition-id or -to-status.
//
// The client dials over plaintext (insecure credentials) because the
// Phase-1 server scope is the loopback interface on a trusted host;
// transport security is the operator's responsibility.
//
// Example:
//
//	gojira-client -address 127.0.0.1:50051 -classify EXAMPLE-1
//	gojira-client -key EXAMPLE-1 -format markdown
//	gojira-client -crawl EXAMPLE-1
//	gojira-client -create-project PROJ -create-type Task -create-summary "fix it" -dry-run
//	gojira-client -comment PROJ-1 -comment-text "looks good"
//	gojira-client -transitions PROJ-1
//	gojira-client -transition PROJ-1 -to-status Done
//
// Errors abort the process with log.Fatal; a non-zero exit code is the
// only failure signal.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
)

func main() {
	address := flag.String("address", "127.0.0.1:50051", "gRPC server address (host:port)")
	classifyInput := flag.String("classify", "", "input to classify (URL or bare issue key)")
	jiraSite := flag.String("jira-site", "", "Jira site override for the Classify call (optional)")
	getIssueKey := flag.String("key", "", "issue key to fetch via GetIssue")
	format := flag.String("format", "markdown", "GetIssue output format: markdown|json|structured")
	crawlSeed := flag.String("crawl", "", "comma-separated start keys for a streaming Crawl")

	// Write subcommands. Each is opt-in: pass the corresponding flag(s)
	// and the binary dispatches to that RPC. Mutually exclusive in the
	// switch below — the first non-empty trigger wins, in order:
	// classify, key, crawl, create-summary, comment, transitions,
	// transition.
	createProject := flag.String("create-project", "", "project key for CreateIssue")
	createType := flag.String("create-type", "Task", "issue type for CreateIssue")
	createSummary := flag.String("create-summary", "", "summary for CreateIssue (presence triggers CreateIssue)")
	createDesc := flag.String("create-description", "", "optional description text for CreateIssue")
	commentKey := flag.String("comment", "", "issue key to add a comment to")
	commentText := flag.String("comment-text", "", "comment body text")
	transitionsKey := flag.String("transitions", "", "issue key to list transitions for")
	transitionKey := flag.String("transition", "", "issue key to transition")
	transitionID := flag.String("transition-id", "", "transition id (use with -transition)")
	transitionToStatus := flag.String("to-status", "", "target status name (use with -transition; resolved server-side)")
	dryRun := flag.Bool("dry-run", false, "for create: ask the server to return the request body without creating")
	flag.Parse()

	conn, err := grpc.NewClient(*address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial %s: %v", *address, err)
	}
	defer conn.Close()

	client := gojirav1.NewGojiraClient(conn)
	ctx := context.Background()

	switch {
	case *classifyInput != "":
		if err := doClassify(ctx, client, *classifyInput, *jiraSite, os.Stdout); err != nil {
			log.Fatalf("classify: %v", err)
		}

	case *getIssueKey != "":
		if err := doGetIssue(ctx, client, *getIssueKey, *format, os.Stdout); err != nil {
			log.Fatalf("get issue: %v", err)
		}

	case *crawlSeed != "" || flag.NArg() > 0:
		keys := collectCrawlKeys(*crawlSeed, flag.Args())
		if err := doCrawl(ctx, client, keys, os.Stdout); err != nil {
			log.Fatalf("crawl: %v", err)
		}

	case *createSummary != "":
		if err := doCreateIssue(ctx, client, *createProject, *createType, *createSummary, *createDesc, *dryRun, os.Stdout); err != nil {
			log.Fatalf("create issue: %v", err)
		}

	case *commentKey != "":
		if err := doAddComment(ctx, client, *commentKey, *commentText, os.Stdout); err != nil {
			log.Fatalf("add comment: %v", err)
		}

	case *transitionsKey != "":
		if err := doListTransitions(ctx, client, *transitionsKey, os.Stdout); err != nil {
			log.Fatalf("list transitions: %v", err)
		}

	case *transitionKey != "":
		if err := doTransition(ctx, client, *transitionKey, *transitionID, *transitionToStatus, os.Stdout); err != nil {
			log.Fatalf("transition: %v", err)
		}

	default:
		flag.Usage()
		os.Exit(2)
	}
}

// collectCrawlKeys flattens the -crawl comma-separated list and any
// positional args into a single ordered slice with empties removed.
func collectCrawlKeys(seed string, positional []string) []string {
	out := make([]string, 0, len(positional)+1)
	for _, k := range strings.Split(seed, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	for _, k := range positional {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func doClassify(ctx context.Context, client gojirav1.GojiraClient, input, site string, out io.Writer) error {
	resp, err := client.Classify(ctx, &gojirav1.ClassifyRequest{
		Input:    input,
		JiraSite: site,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Kind:     %s\n", resp.GetKind())
	fmt.Fprintf(out, "IssueKey: %s\n", resp.GetIssueKey())
	fmt.Fprintf(out, "Owner:    %s\n", resp.GetOwner())
	fmt.Fprintf(out, "Repo:     %s\n", resp.GetRepo())
	fmt.Fprintf(out, "PRNumber: %d\n", resp.GetPrNumber())
	fmt.Fprintf(out, "Url:      %s\n", resp.GetUrl())
	return nil
}

func doGetIssue(ctx context.Context, client gojirav1.GojiraClient, key, formatStr string, out io.Writer) error {
	fmt_ := parseOutputFormat(formatStr)
	resp, err := client.GetIssue(ctx, &gojirav1.GetIssueRequest{
		Key:    key,
		Format: fmt_,
	})
	if err != nil {
		return err
	}
	switch fmt_ {
	case gojirav1.OutputFormat_OUTPUT_FORMAT_MARKDOWN:
		fmt.Fprintln(out, resp.GetMarkdown())
	case gojirav1.OutputFormat_OUTPUT_FORMAT_JSON:
		fmt.Fprintln(out, resp.GetJson())
	default:
		issue := resp.GetIssue()
		fmt.Fprintf(out, "Key:       %s\n", issue.GetKey())
		fmt.Fprintf(out, "Summary:   %s\n", issue.GetSummary())
		fmt.Fprintf(out, "Status:    %s\n", issue.GetStatus())
		fmt.Fprintf(out, "IssueType: %s\n", issue.GetIssueType())
		fmt.Fprintf(out, "Parent:    %s\n", issue.GetParentKey())
		fmt.Fprintf(out, "Children:  %v\n", issue.GetChildren())
		fmt.Fprintf(out, "References: %d\n", len(issue.GetReferences()))
	}
	return nil
}

func doCrawl(ctx context.Context, client gojirav1.GojiraClient, startKeys []string, out io.Writer) error {
	stream, err := client.Crawl(ctx, &gojirav1.CrawlRequest{StartKeys: startKeys})
	if err != nil {
		return err
	}
	for {
		evt, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "[%s] %s: %s\n",
			evt.GetKind().String(),
			evt.GetIssueKey(),
			evt.GetMessage())
	}
}

// parseOutputFormat maps a -format flag value to the wire enum. Unknown
// values fall back to STRUCTURED so users get something predictable
// even when they typo the flag.
func parseOutputFormat(s string) gojirav1.OutputFormat {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "markdown", "md":
		return gojirav1.OutputFormat_OUTPUT_FORMAT_MARKDOWN
	case "json":
		return gojirav1.OutputFormat_OUTPUT_FORMAT_JSON
	default:
		return gojirav1.OutputFormat_OUTPUT_FORMAT_STRUCTURED
	}
}

// ---------------------------------------------------------------------------
// Write subcommands (Phase 2)
// ---------------------------------------------------------------------------

// doCreateIssue invokes CreateIssue with the supplied project/type/summary
// (and optional description). When dryRun is true the server returns the
// JSON body it would have POSTed and the client prints it instead of the
// usual key/id/self triplet.
func doCreateIssue(ctx context.Context, client gojirav1.GojiraClient, project, issueType, summary, description string, dryRun bool, out io.Writer) error {
	resp, err := client.CreateIssue(ctx, &gojirav1.CreateIssueRequest{
		Project:     project,
		IssueType:   issueType,
		Summary:     summary,
		Description: description,
		DryRun:      dryRun,
	})
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(out, "DryRun body:\n%s\n", string(resp.GetDryRunBody()))
		return nil
	}
	fmt.Fprintf(out, "Key:  %s\n", resp.GetKey())
	fmt.Fprintf(out, "ID:   %s\n", resp.GetId())
	fmt.Fprintf(out, "Self: %s\n", resp.GetSelf())
	return nil
}

// doAddComment invokes AddComment with a plain-text body. The server
// converts the text to ADF; the printed response is Jira's id + author
// + timestamp.
func doAddComment(ctx context.Context, client gojirav1.GojiraClient, key, text string, out io.Writer) error {
	resp, err := client.AddComment(ctx, &gojirav1.AddCommentRequest{
		Key:      key,
		BodyText: text,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ID:     %s\n", resp.GetId())
	fmt.Fprintf(out, "Author: %s\n", resp.GetAuthorDisplayName())
	fmt.Fprintf(out, "Time:   %s\n", resp.GetCreated())
	return nil
}

// doListTransitions invokes ListTransitions and prints each available
// transition as id/name/to-status. The set is workflow-state dependent;
// Jira only returns transitions whose preconditions are met.
func doListTransitions(ctx context.Context, client gojirav1.GojiraClient, key string, out io.Writer) error {
	resp, err := client.ListTransitions(ctx, &gojirav1.ListTransitionsRequest{Key: key})
	if err != nil {
		return err
	}
	for _, t := range resp.GetTransitions() {
		fmt.Fprintf(out, "%s\t%s\t->\t%s\n", t.GetId(), t.GetName(), t.GetToStatus())
	}
	return nil
}

// doTransition invokes TransitionIssue. Provide EITHER transitionID OR
// toStatus (case-insensitive name resolution happens server-side via
// the same ListTransitions endpoint).
func doTransition(ctx context.Context, client gojirav1.GojiraClient, key, transitionID, toStatus string, out io.Writer) error {
	resp, err := client.TransitionIssue(ctx, &gojirav1.TransitionIssueRequest{
		Key:              key,
		TransitionId:     transitionID,
		TargetStatusName: toStatus,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ok: %v\n", resp.GetOk())
	return nil
}
