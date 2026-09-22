// Command kibitz-mcp serves one review job's facts to the agent.
//
// It runs as an MCP server over stdio, which is how opencode starts it, and as
// a plain CLI, which is how a person debugging a review — or a different agent
// engine — asks it the same questions. Both go through the same handlers, so
// the two cannot drift apart.
//
//	kibitz-mcp --context /path/context.json            # MCP server on stdio
//	kibitz-mcp --context /path/context.json tools      # list the tools
//	kibitz-mcp --context /path/context.json call get_pr_diff '{"path":"queue.go"}'
//
// It holds no credential and cannot reach the forge. Everything it answers was
// fetched by the worker before the agent started and written to a file, so a
// tool call is a local read and nothing the agent is talked into doing can
// reach further than that.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/yteraoka/kibitz/internal/jobcontext"
	"github.com/yteraoka/kibitz/internal/mcp"
)

// version is set at build time.
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kibitz-mcp: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("kibitz-mcp", flag.ContinueOnError)
	flags.SetOutput(stdout)
	contextPath := flags.String("context", "", "path to the job's context file")
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}
	if *contextPath == "" {
		return errors.New("--context is required")
	}

	job, err := jobcontext.Load(*contextPath)
	if err != nil {
		return err
	}
	server := newServer(job)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rest := flags.Args()
	switch {
	case len(rest) == 0:
		return server.Serve(ctx, stdin, stdout)
	case rest[0] == "tools":
		for _, tool := range server.Tools {
			fmt.Fprintf(stdout, "%-20s %s\n", tool.Name, firstLine(tool.Description))
		}
		return nil
	case rest[0] == "call":
		return callTool(ctx, stdout, server, rest[1:])
	default:
		return fmt.Errorf("unknown command %q; expected \"tools\" or \"call\"", rest[0])
	}
}

func callTool(ctx context.Context, stdout io.Writer, server *mcp.Server, args []string) error {
	if len(args) == 0 {
		return errors.New("call needs a tool name")
	}
	arguments := json.RawMessage("{}")
	if len(args) > 1 {
		arguments = json.RawMessage(args[1])
	}

	text, err := server.Call(ctx, args[0], arguments)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, text)
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
