// Command skyhookd is the landside Skyhook server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
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
		demo       = flag.Bool("demo", false,
			"loopback demo: plain HTTP on 127.0.0.1, no TLS, no QUIC, no pairing certificate")
		demoFor = flag.Duration("demo-for", 0, "with -demo, stop after this long (0 = until Ctrl-C)")
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
	if *demo {
		cfg = demoConfig(cfg)
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

	if *demo && *demoFor > 0 {
		// A demo that cleans up after itself: this mode has no TLS, so leaving
		// it running by accident is exactly what should not happen.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *demoFor)
		defer cancel()
		log.Info("demo will stop on its own", "after", demoFor.String())
	}

	if *initOnly {
		// No browser: this only has to leave behind a data directory, a
		// certificate and a pairing file.
		cert, err := server.Prepare(cfg, log)
		if err != nil {
			fatal("init", err)
		}
		log.Info("initialised",
			"dataDir", cfg.DataDir,
			"pairing", cfg.PairingPath(),
			"fingerprint", cert.FingerprintHex())
		return
	}

	srv, err := server.New(ctx, cfg, log)
	if err != nil {
		fatal("startup", err)
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

// demoConfig points a configuration at this machine only. Anything the demo
// leaves at its default would otherwise be a public-facing default.
func demoConfig(cfg config.Config) config.Config {
	cfg.InsecureLoopback = true
	cfg.WebSocketFallback = true
	cfg.Hosts = []string{"127.0.0.1"}
	if isDefaultAddr(cfg.Listen) {
		cfg.Listen = "127.0.0.1:4433"
	}
	if isDefaultAddr(cfg.FallbackListen) {
		cfg.FallbackListen = "127.0.0.1:4434"
	}
	return cfg
}

// isDefaultAddr reports whether an address is an unset "all interfaces" one,
// so an explicit -config choice is never quietly overridden.
func isDefaultAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err != nil || host == "" || host == "0.0.0.0" || host == "::"
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
