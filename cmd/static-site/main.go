// Command static-site serves one OCI artifact holding a built static site.
// See docs/static-site.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/truvity/edge/v4/internal/staticsite"
)

// version is stamped by the release build.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level(os.Getenv("STATIC_SITE_LOG_LEVEL"))}))
	cfg, err := staticsite.FromEnv(os.Getenv)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	log.Info("starting", "version", version, "addr", cfg.Addr, "ops_addr", cfg.OpsAddr, "cache_private", cfg.CachePrivate)
	// Names only: a value is never logged.
	log.Info("placeholders", "names", cfg.PlaceholderNames(), "strict", cfg.PlaceholderStrict)
	if cfg.LiveDir != "" {
		log.Info("live files", "dir", cfg.LiveDir, "files", cfg.LiveFiles, "interval", cfg.LiveInterval.String(), "max_bytes", cfg.LiveMaxBytes)
	}
	if err := staticsite.NewServer(&cfg, log).Run(context.Background()); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func level(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
