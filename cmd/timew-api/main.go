// Command timew-api serves an HTTP API and web page for timewarrior.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lurraca/timew-api/internal/auth"
	"github.com/lurraca/timew-api/internal/server"
	"github.com/lurraca/timew-api/internal/timew"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	home, _ := os.UserHomeDir()
	port, _ := strconv.Atoi(env("TIMEW_API_PORT", "8097"))
	var (
		host     = flag.String("host", "127.0.0.1", "address to listen on")
		portFlag = flag.Int("port", port, "port to listen on")
		db       = flag.String("db", env("TIMEWARRIORDB", filepath.Join(home, ".local/share/timewarrior")), "timewarrior database")
		team     = flag.String("team-domain", os.Getenv("TIMEW_API_CF_TEAM_DOMAIN"), "Cloudflare Access team domain, e.g. myteam.cloudflareaccess.com")
		aud      = flag.String("audience", os.Getenv("TIMEW_API_CF_AUD"), "Access application AUD tag")
		project  = flag.String("project", os.Getenv("TIMEW_API_PROJECT"), "label in the weekly report header")
		noAuth   = flag.Bool("no-auth", false, "development only; refused unless bound to loopback")
	)
	flag.Parse()

	var id auth.Identity
	switch {
	case *noAuth:
		if ip := net.ParseIP(*host); *host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			log.Fatal("--no-auth is only allowed on a loopback address")
		}
		id = auth.Dev{}
	case *team != "" && *aud != "":
		id = &auth.Access{TeamDomain: *team, Audience: *aud}
	default:
		log.Fatal("set --team-domain and --audience (or TIMEW_API_CF_*), or --no-auth for local development")
	}

	runner, err := timew.New(*db)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              net.JoinHostPort(*host, strconv.Itoa(*portFlag)),
		Handler:           (&server.Server{Timew: runner, Identity: id, Project: *project}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	auth := "Access"
	if *noAuth {
		auth = "OFF"
	}
	fmt.Fprintf(os.Stderr, "timew-api on http://%s (db %s, auth %s)\n", srv.Addr, *db, auth)
	log.Fatal(srv.ListenAndServe())
}
