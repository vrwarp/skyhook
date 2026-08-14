// Command skyhookd is the landside Skyhook server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vrwarp/skyhook/internal/config"
	"github.com/vrwarp/skyhook/internal/server"
	"github.com/vrwarp/skyhook/internal/session"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		configPath = flag.String("config", envOr("SKYHOOK_CONFIG", ""), "path to config file")
		printVer   = flag.Bool("version", false, "print version and exit")
		showPair   = flag.Bool("pair", false, "print the pairing file and exit")
		initOnly   = flag.Bool("init", false, "create data dir, token and certificate, then exit")
	)
	flag.Parse()

	if *printVer {
		fmt.Println("skyhookd", version)
		return
	}
	session.SetVersion(version)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("config", err)
	}
	log := newLogger(cfg.LogLevel)

	if cfg.Token == "" {
		cfg.Token = session.NewToken()
		if *configPath != "" {
			if err := cfg.Save(*configPath); err != nil {
				log.Warn("could not persist generated token", "err", err)
			}
		}
		log.Info("generated pairing token", "token", cfg.Token)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg, log)
	if err != nil {
		fatal("startup", err)
	}
	if *initOnly {
		if err := srv.WritePairingFile(); err != nil {
			log.Error("could not write the pairing file", "err", err)
		}
		log.Info("initialised",
			"dataDir", cfg.DataDir,
			"pairing", cfg.PairingPath(),
			"fingerprint", srv.Cert().FingerprintHex())
		_ = srv.Shutdown()
		return
	}
	if *showPair {
		p, err := config.ReadPairing(cfg.PairingPath())
		if err == nil {
			fmt.Printf("%+v\n", p)
		}
		_ = srv.Shutdown()
		return
	}

	log.Info("skyhook server starting",
		"version", version, "listen", cfg.Listen, "fallback", cfg.FallbackListen,
		"profile", cfg.ProfileDir(), "pairing", cfg.PairingPath())

	if err := srv.Start(ctx); err != nil {
		fatal("serve", err)
	}
	log.Info("skyhook server stopped")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "skyhookd: %s: %v\n", what, err)
	os.Exit(1)
}
