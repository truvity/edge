// Command docs-mcp builds and serves an MCP pack: a documentation site
// turned into a versioned directory that agents search and read over MCP.
// See docs/docs-mcp.md.
//
//	docs-mcp pack --in <dir> --out <dir>   build a pack (in CI)
//	docs-mcp serve                         serve a pack (DOCS_MCP_* environment)
//	docs-mcp version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/truvity/edge/v4/internal/docsmcp"
	"github.com/truvity/edge/v4/internal/docspack"
)

// version is stamped by the release build.
var version = "dev"

const usage = `usage:
  docs-mcp pack --in <dir> --out <dir> [--title <text>]
  docs-mcp serve
  docs-mcp version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "pack":
		return pack(args[1:], stdout, stderr, getenv)
	case "serve":
		return serve(stderr, getenv)
	case "version", "-version", "--version":
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "-help", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "docs-mcp: unknown command %q\n%s", args[0], usage)
	return 2
}

func pack(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("docs-mcp pack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "directory of Markdown (and, optionally, llms.txt and llms-full.txt at its top)")
	out := fs.String("out", "", "pack directory to write; must not exist or be empty")
	title := fs.String("title", "", "name of the documentation set, shown to clients (optional)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *in == "" || *out == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "docs-mcp pack: --in and --out are required, and nothing else is taken")
		return 2
	}
	opt := docspack.BuildOptions{In: *in, Out: *out, Title: *title}
	// A reproducible build: the pack records a time only when the caller sets
	// one, by the SOURCE_DATE_EPOCH convention.
	if v := getenv("SOURCE_DATE_EPOCH"); v != "" {
		sec, err := strconv.ParseInt(v, 10, 64)
		if err != nil || sec < 0 {
			_, _ = fmt.Fprintf(stderr, "docs-mcp pack: SOURCE_DATE_EPOCH %q is not a non-negative integer\n", v)
			return 2
		}
		opt.CreatedAt = time.Unix(sec, 0)
	}
	sum, err := docspack.Build(opt)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "docs-mcp pack: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "packed %d documents, %d sections, %d terms, %d bytes (format %s version %d) into %s\n",
		sum.Docs, sum.Sections, sum.Terms, sum.Bytes, docspack.Format, docspack.FormatVersion, *out)
	return 0
}

func serve(stderr io.Writer, getenv func(string) string) int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level(getenv("DOCS_MCP_LOG_LEVEL"))}))
	cfg, err := docsmcp.FromEnv(getenv)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		_, _ = fmt.Fprintln(stderr, "docs-mcp serve: invalid configuration")
		return 2
	}
	log.Info("starting", "version", version, "addr", cfg.Addr, "ops_addr", cfg.OpsAddr, "pack_format_version", docspack.FormatVersion)
	if err := docsmcp.New(&cfg, version, log).Run(context.Background()); err != nil {
		log.Error("server stopped", "error", err)
		return 1
	}
	return 0
}

func level(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
