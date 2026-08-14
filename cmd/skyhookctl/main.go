// Command skyhookctl is the headless control and diagnostics client. It speaks
// the real protocol, so it doubles as the end-to-end probe used by CI and by
// the link-emulation harness.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/vrwarp/skyhook/internal/client"
	"github.com/vrwarp/skyhook/internal/config"
	"github.com/vrwarp/skyhook/internal/protocol"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "probe":
		probe(args)
	case "pairing":
		pairing(args)
	case "kill":
		kill(args)
	case "chat":
		chat(args)
	case "capture":
		capture(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "skyhookctl: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `skyhookctl - Skyhook control and diagnostics

  probe    open a URL through the mirror and report what arrived
  pairing  print the pairing file the client needs (-link for the pairing URL)
  kill     wipe the landside session and browser profile
  chat     drive the chat adapter (list, send)
  capture  take a diagnostic bundle of both halves of a mirrored tab

Run "skyhookctl <command> -h" for flags.
`)
}

type common struct {
	url     string
	token   string
	pairing string
	zstd    bool
}

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.url, "server", envOr("SKYHOOK_URL", "wss://127.0.0.1:4434/skyhook"), "server websocket URL")
	fs.StringVar(&c.token, "token", os.Getenv("SKYHOOK_TOKEN"), "pairing token")
	fs.StringVar(&c.pairing, "pairing", "", "read server URL and token from a pairing file")
	fs.BoolVar(&c.zstd, "zstd", true, "negotiate zstd compression")
}

func (c *common) dial(ctx context.Context) *client.Client {
	if c.pairing != "" {
		p, err := config.ReadPairing(c.pairing)
		if err != nil {
			log.Fatalf("pairing: %v", err)
		}
		if p.Fallback != "" {
			c.url = p.Fallback
		}
		if c.token == "" {
			c.token = p.Token
		}
	}
	cl, err := client.Dial(ctx, c.url, client.Options{
		Token: c.token, Zstd: c.zstd, Insecure: true,
		Viewport: protocol.Viewport{W: 1280, H: 900, DPR: 1},
		Logger:   logger{},
	})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	return cl
}

type logger struct{}

func (logger) Printf(f string, a ...any) { log.Printf(f, a...) }

func probe(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	var c common
	c.flags(fs)
	url := fs.String("url", "https://news.ycombinator.com/", "page to open")
	expect := fs.String("expect", "", "fail unless the mirrored text contains this")
	timeout := fs.Duration("timeout", 60*time.Second, "overall timeout")
	dump := fs.String("dump", "", "write the mirrored HTML here")
	clickText := fs.String("click", "", "after load, click the element containing this text")
	typeText := fs.String("type", "", "after load, type this into the first editable node")
	jsonOut := fs.Bool("json", false, "emit a machine-readable report")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	cl := c.dial(ctx)
	defer func() { _ = cl.Close() }()

	start := time.Now()
	if err := cl.OpenTab(*url); err != nil {
		log.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, 30*time.Second)
	if err != nil {
		log.Fatalf("tab: %v", err)
	}
	if err := cl.Navigate(tab, *url); err != nil {
		log.Fatalf("navigate: %v", err)
	}

	report := map[string]any{"url": *url, "tab": tab}
	if *expect != "" {
		if err := cl.WaitForText(ctx, tab, *expect, *timeout); err != nil {
			log.Fatalf("expect: %v", err)
		}
		report["firstUsefulPaintMs"] = time.Since(start).Milliseconds()
	} else {
		// No assertion: wait for the first snapshot so there is something to
		// report on.
		deadline := time.Now().Add(*timeout)
		for time.Now().Before(deadline) && cl.Model(tab) == nil {
			time.Sleep(50 * time.Millisecond)
		}
		report["firstSnapshotMs"] = time.Since(start).Milliseconds()
	}

	if *clickText != "" {
		n, err := cl.FindByText(tab, *clickText)
		if err != nil {
			log.Fatalf("click: %v", err)
		}
		clickAt := time.Now()
		if err := cl.Click(tab, n.ID); err != nil {
			log.Fatalf("click: %v", err)
		}
		report["clickNode"] = n.ID
		report["clickLatencyMs"] = time.Since(clickAt).Milliseconds()
		time.Sleep(2 * time.Second)
	}
	if *typeText != "" {
		m := cl.Model(tab)
		var target int64
		for id, n := range m.Nodes {
			if n.Flags&protocol.FlagEditable != 0 {
				target = id
				break
			}
		}
		if target == 0 {
			log.Fatal("type: no editable node in the mirror")
		}
		if err := cl.Type(tab, target, *typeText); err != nil {
			log.Fatalf("type: %v", err)
		}
		time.Sleep(2 * time.Second)
		report["typedInto"] = target
	}

	m := cl.Model(tab)
	if m != nil {
		report["nodes"] = len(m.Nodes)
		report["cssRules"] = len(m.CSS)
		report["title"] = m.Title
		report["textBytes"] = len(m.Text())
		if *dump != "" {
			if err := os.WriteFile(*dump, []byte(m.HTML()), 0o600); err != nil {
				log.Fatalf("dump: %v", err)
			}
		}
	}
	sent, recv := cl.BytesTransferred()
	report["bytesSent"] = sent
	report["bytesReceived"] = recv
	report["images"] = len(cl.Images())
	report["elapsedMs"] = time.Since(start).Milliseconds()

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	keys := []string{"url", "tab", "title", "nodes", "cssRules", "textBytes",
		"images", "firstUsefulPaintMs", "firstSnapshotMs", "clickLatencyMs",
		"bytesSent", "bytesReceived", "elapsedMs"}
	for _, k := range keys {
		if v, ok := report[k]; ok {
			fmt.Printf("%-20s %v\n", k, v)
		}
	}
}

func pairing(args []string) {
	fs := flag.NewFlagSet("pairing", flag.ExitOnError)
	path := fs.String("file", defaultPairingPath(), "pairing file")
	// The server logs the link once at startup. Behind a reverse proxy — or on
	// any long-running deployment — that line has usually scrolled away by the
	// time a new device needs pairing.
	link := fs.Bool("link", false, "print the pairing link instead of the file")
	_ = fs.Parse(args)
	p, err := config.ReadPairing(*path)
	if err != nil {
		log.Fatalf("pairing: %v", err)
	}
	if *link {
		fmt.Println(p.Link())
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(p)
}

func kill(args []string) {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	var c common
	c.flags(fs)
	yes := fs.Bool("yes", false, "confirm: this wipes cookies and logins landside")
	_ = fs.Parse(args)
	if !*yes {
		log.Fatal("kill: refusing without -yes (this deletes the landside browser profile)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cl := c.dial(ctx)
	defer func() { _ = cl.Close() }()
	if err := cl.AdapterCommand(protocol.AdapterCommand{}); err != nil {
		_ = err // best effort; the kill frame below is what matters
	}
	if err := cl.Kill(); err != nil {
		log.Fatalf("kill: %v", err)
	}
	fmt.Println("kill switch sent")
}

func chat(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	var c common
	c.flags(fs)
	space := fs.String("space", "", "space id to send to")
	text := fs.String("send", "", "message to send")
	list := fs.Bool("list", false, "list spaces and recent messages")
	wait := fs.Duration("wait", 20*time.Second, "how long to collect records")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()
	cl := c.dial(ctx)
	defer func() { _ = cl.Close() }()

	if *text != "" {
		if err := cl.AdapterCommand(protocol.AdapterCommand{
			Adapter: "googlechat", Cmd: "send", Space: *space, Text: *text,
			LocalID: fmt.Sprintf("local-%d", time.Now().UnixNano()),
		}); err != nil {
			log.Fatalf("send: %v", err)
		}
		fmt.Println("sent")
	}
	if *list || *text == "" {
		if err := cl.AdapterCommand(protocol.AdapterCommand{
			Adapter: "googlechat", Cmd: "sync",
		}); err != nil {
			log.Fatalf("sync: %v", err)
		}
		time.Sleep(*wait)
		for _, r := range cl.AdapterRecords() {
			switch r.Kind {
			case "space":
				fmt.Printf("space  %-24s %s (unread %d)\n", r.ID, r.Text, r.Unread)
			case "message", "sent":
				fmt.Printf("msg    %-24s %s: %s\n", r.Space, r.Author, truncate(r.Text, 80))
			}
		}
	}
}

// capture takes a diagnostic bundle. Given a -url it opens that page first, so
// one command reproduces a rendering complaint and captures it: this client
// keeps the same DOM replica the real patcher builds, so a divergence it can
// reproduce is a divergence in the frames rather than in a browser.
func capture(args []string) {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	var c common
	c.flags(fs)
	url := fs.String("url", "", "open this page first, and capture it once it has settled")
	note := fs.String("note", "", "what looks wrong (goes into the bundle)")
	reason := fs.String("reason", protocol.CaptureManual, "why: manual, divergence, resync")
	settle := fs.Duration("settle", 8*time.Second, "how long to let the page settle before capturing")
	timeout := fs.Duration("timeout", 3*time.Minute, "overall timeout")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	cl := c.dial(ctx)
	defer func() { _ = cl.Close() }()

	if *url != "" {
		if err := cl.OpenTab(*url); err != nil {
			log.Fatalf("open tab: %v", err)
		}
		tab, err := cl.WaitForTab(ctx, 30*time.Second)
		if err != nil {
			log.Fatalf("tab: %v", err)
		}
		if err := cl.Navigate(tab, *url); err != nil {
			log.Fatalf("navigate: %v", err)
		}
		time.Sleep(*settle)
	}

	if err := cl.Capture(*reason, *note); err != nil {
		log.Fatalf("capture: %v", err)
	}
	done, err := cl.WaitForCapture(ctx, *timeout)
	if err != nil {
		log.Fatalf("capture: %v", err)
	}
	if done.Error != "" {
		log.Fatalf("capture: the server refused: %s", done.Error)
	}
	// The path is the server's, not this machine's: the bundle is landside on
	// purpose, and saying so avoids a hunt for a file that was never here.
	fmt.Printf("capture %s written on the server: %s (%d bytes)\n", done.ID, done.Path, done.Bytes)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func defaultPairingPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "pairing.json"
	}
	return home + "/.skyhook/pairing.json"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
