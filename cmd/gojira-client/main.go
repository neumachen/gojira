// Command gojira-client is a tiny reference client for the gojira gRPC
// server, intended as a smoke / interoperability tool, not a production
// frontend.
//
// It exercises three RPCs:
//
//   - Classify  — pass any URL or bare issue key via -classify.
//   - GetIssue  — pass an issue key via -key (with -format=markdown|json|structured).
//   - Crawl     — pass one or more start keys as positional args, or via -crawl.
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
