// sentinel-server is the central audit/policy/approval service.
//
// Slice v0.2.1 scope: bearer-token-authenticated audit event ingest from
// fleet agents, multi-agent dashboard API, single-binary deployment with
// SQLite storage. Self-hosted by design — no SaaS component.
//
// Usage:
//
//	sentinel-server serve [--addr ADDR] [--data DIR]
//	sentinel-server agent create <name>      # generates a token (printed once)
//	sentinel-server agent list
//	sentinel-server agent delete <id>
//	sentinel-server version
//	sentinel-server help
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ironmark056/sentinel/internal/server"
)

const version = "0.2.0-dev"

func main() {
	server.Version = version

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serveCmd(os.Args[2:])
	case "agent":
		agentCmd(os.Args[2:])
	case "enroll":
		enrollCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("sentinel-server %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `sentinel-server %s — self-hosted central service for Sentinel agents

Usage:
  sentinel-server serve [--addr ADDR] [--data DIR]
      Run the HTTP service.
      Default addr: %s
      Default data dir: ./data
      Admin token (gates dashboard write endpoints): set
        env SENTINEL_ADMIN_TOKEN. Unset is fine for VPN-only deployments.

  sentinel-server agent create <name> [--data DIR] [--meta key=value,...]
      Register a new agent and print its bearer token. The token is
      shown ONCE; only its hash is stored.

  sentinel-server agent list [--data DIR]
      List registered agents.

  sentinel-server agent delete <id> [--data DIR]
      Revoke an agent and remove all its events.

  sentinel-server enroll create <name> [--data DIR] [--meta k=v,...]
                                       [--ttl 24h] [--base-url URL]
      Generate a one-time enrollment URL for an employee. Single-use
      and expires (default 24h). Prints the exact 'sentinel enroll <url>'
      command to send. --base-url overrides the host the URL is built
      with (e.g. when the server sits behind a reverse proxy on a
      different hostname than --addr).

  sentinel-server enroll list [--data DIR]
      List outstanding and consumed enrollments.

  sentinel-server enroll revoke <id> [--data DIR]
      Revoke an outstanding enrollment.

  sentinel-server version
  sentinel-server help
`, version, server.DefaultAddr)
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", server.DefaultAddr, "listen address")
	dataDir := fs.String("data", "./data", "data directory (SQLite DB lives here)")
	_ = fs.Parse(args)

	srv, err := server.New(server.Options{
		Addr:       *addr,
		DataDir:    *dataDir,
		AdminToken: os.Getenv("SENTINEL_ADMIN_TOKEN"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "server init: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	if err := srv.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "server run: %v\n", err)
		os.Exit(1)
	}
}

func agentCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "agent: subcommand required (create | list | delete)")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		agentCreate(args[1:])
	case "list":
		agentList(args[1:])
	case "delete":
		agentDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown agent subcommand: %s\n", args[0])
		os.Exit(2)
	}
}

func agentCreate(args []string) {
	fs := flag.NewFlagSet("agent create", flag.ExitOnError)
	dataDir := fs.String("data", "./data", "data directory")
	metaFlag := fs.String("meta", "", "comma-separated key=value metadata pairs")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "agent create: <name> required")
		os.Exit(2)
	}
	name := rest[0]

	store, err := server.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	meta := parseMeta(*metaFlag)
	token, agent, err := store.CreateAgent(context.Background(), name, meta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Agent created: id=%d name=%s\n", agent.ID, agent.Name)
	fmt.Println()
	fmt.Println("Bearer token (save this — it will not be shown again):")
	fmt.Println()
	fmt.Println("  " + token)
	fmt.Println()
	fmt.Println("Put this in the agent's sentinel.yaml under central.token.")
}

func agentList(args []string) {
	fs := flag.NewFlagSet("agent list", flag.ExitOnError)
	dataDir := fs.String("data", "./data", "data directory")
	asJSON := fs.Bool("json", false, "output JSON")
	_ = fs.Parse(args)

	store, err := server.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	list, err := store.ListAgents(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(list)
		return
	}
	if len(list) == 0 {
		fmt.Println("No agents registered.")
		return
	}
	fmt.Printf("%-6s  %-30s  %-25s  %s\n", "ID", "NAME", "CREATED", "LAST SEEN")
	for _, a := range list {
		seen := "(never)"
		if !a.LastSeen.IsZero() {
			seen = a.LastSeen.Format("2006-01-02 15:04:05")
		}
		fmt.Printf("%-6d  %-30s  %-25s  %s\n",
			a.ID, a.Name, a.Created.Format("2006-01-02 15:04:05"), seen)
	}
}

func agentDelete(args []string) {
	fs := flag.NewFlagSet("agent delete", flag.ExitOnError)
	dataDir := fs.String("data", "./data", "data directory")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "agent delete: <id> required")
		os.Exit(2)
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad id: %v\n", err)
		os.Exit(2)
	}

	store, err := server.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := store.DeleteAgent(context.Background(), id); err != nil {
		fmt.Fprintf(os.Stderr, "delete: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Agent %d deleted.\n", id)
}

// ---------------------------------------------------------------------------
// enroll
// ---------------------------------------------------------------------------

func enrollCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "enroll: subcommand required (create | list | revoke)")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		enrollCreate(args[1:])
	case "list":
		enrollList(args[1:])
	case "revoke":
		enrollRevoke(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown enroll subcommand: %s\n", args[0])
		os.Exit(2)
	}
}

func enrollCreate(args []string) {
	fs := flag.NewFlagSet("enroll create", flag.ExitOnError)
	dataDir := fs.String("data", "./data", "data directory")
	metaFlag := fs.String("meta", "", "comma-separated key=value metadata pairs")
	ttlFlag := fs.Duration("ttl", 24*time.Hour, "how long the enrollment URL is valid")
	baseURL := fs.String("base-url", "", "public base URL the URL is built with (e.g. https://central.acme.internal). Defaults to inferring from --addr/host.")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "enroll create: <name> required")
		os.Exit(2)
	}
	name := rest[0]

	store, err := server.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	meta := parseMeta(*metaFlag)
	ott, en, err := store.CreateEnrollment(context.Background(), name, *ttlFlag, meta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create: %v\n", err)
		os.Exit(1)
	}

	base := *baseURL
	if base == "" {
		base = "https://" + defaultHost()
	}
	url := base + "/e/" + ott

	fmt.Printf("Enrollment created: id=%d name=%s\n", en.ID, en.Name)
	fmt.Printf("Expires:            %s\n", en.Expires.Format("2006-01-02 15:04:05"))
	fmt.Println()
	fmt.Println("Send the employee this single command:")
	fmt.Println()
	fmt.Println("  sentinel enroll " + url)
	fmt.Println()
	fmt.Println("Single-use. If --base-url didn't match your reverse proxy's hostname,")
	fmt.Println("substitute the correct host in the URL above before sending.")
}

func enrollList(args []string) {
	fs := flag.NewFlagSet("enroll list", flag.ExitOnError)
	dataDir := fs.String("data", "./data", "data directory")
	asJSON := fs.Bool("json", false, "output JSON")
	_ = fs.Parse(args)

	store, err := server.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	list, err := store.ListEnrollments(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(list)
		return
	}
	if len(list) == 0 {
		fmt.Println("No enrollments.")
		return
	}
	fmt.Printf("%-6s  %-25s  %-20s  %-20s  %s\n", "ID", "NAME", "EXPIRES", "CONSUMED", "AGENT ID")
	for _, e := range list {
		consumed := "(outstanding)"
		if !e.Consumed.IsZero() {
			consumed = e.Consumed.Format("2006-01-02 15:04:05")
		}
		agent := ""
		if e.ResolvedID > 0 {
			agent = strconv.FormatInt(e.ResolvedID, 10)
		}
		fmt.Printf("%-6d  %-25s  %-20s  %-20s  %s\n",
			e.ID, e.Name,
			e.Expires.Format("2006-01-02 15:04:05"),
			consumed, agent)
	}
}

func enrollRevoke(args []string) {
	fs := flag.NewFlagSet("enroll revoke", flag.ExitOnError)
	dataDir := fs.String("data", "./data", "data directory")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "enroll revoke: <id> required")
		os.Exit(2)
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad id: %v\n", err)
		os.Exit(2)
	}

	store, err := server.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := store.RevokeEnrollment(context.Background(), id); err != nil {
		fmt.Fprintf(os.Stderr, "revoke: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Enrollment %d revoked.\n", id)
}

// defaultHost returns the operator's hostname as a fallback when --base-url
// isn't passed. Almost always a placeholder the admin will need to override
// in production; we emit a friendly one rather than the literal listen addr
// so the printed `sentinel enroll <url>` is at least cleanly substitutable.
func defaultHost() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "sentinel.example.internal"
}

func parseMeta(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, kv := range splitTrim(s, ",") {
		eq := -1
		for i, c := range kv {
			if c == '=' {
				eq = i
				break
			}
		}
		if eq <= 0 {
			continue
		}
		out[kv[:eq]] = kv[eq+1:]
	}
	return out
}

func splitTrim(s, sep string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if string(c) == sep {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
