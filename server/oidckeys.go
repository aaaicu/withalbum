package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/oidc"
)

// runOIDCKeys is `withalbum-server oidc-keys -out DIR` (M62): it downloads
// the Google and Apple sign-in key sets from their fixed addresses, checks
// them and writes <DIR>/google.json and <DIR>/apple.json for a server that
// reads keys from files (-oidc-keys-dir) instead of reaching the internet.
// In compose.yaml it is the "keys" container: the only container with a
// route out, sharing a key volume — not a network — with the server.
//
// It contacts nothing but oidc.KeysURLs, listens on nothing and keeps the
// old file when a download fails.
func runOIDCKeys(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("oidc-keys", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "Directory to write google.json and apple.json into (required)")
	every := fs.Duration("every", 6*time.Hour, "Refresh interval (minimum 10m)")
	once := fs.Bool("once", false, "Fetch once and exit (exit code 1 if any provider failed)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" || fs.NArg() > 0 || *every < 10*time.Minute {
		fmt.Fprintln(stderr, "usage: withalbum-server oidc-keys -out <dir> [-every 6h] [-once]")
		return exitUsage
	}
	if st, err := os.Stat(*out); err != nil || !st.IsDir() {
		fmt.Fprintf(stderr, "oidc-keys: %s is not a directory\n", *out)
		return exitUsage
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		// Never follow a redirect to another address.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		failed := refreshOIDCKeys(ctx, client, *out)
		if *once {
			if failed > 0 {
				return exitProblems
			}
			return 0
		}
		wait := *every
		if failed > 0 {
			wait = 5 * time.Minute // retry sooner; the server keeps using the old file
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(wait):
		}
	}
}

// refreshOIDCKeys fetches and writes every provider's key set and returns
// the number of failures.
func refreshOIDCKeys(ctx context.Context, client *http.Client, dir string) int {
	failed := 0
	for _, p := range oidc.Providers {
		raw, err := oidc.FetchKeySet(ctx, client, p)
		if err == nil {
			err = oidc.WriteKeySet(dir, p, raw)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return failed
			}
			failed++
			log.Printf("oidc-keys: %s: %v (keeping the previous file)", p, err)
			continue
		}
		log.Printf("oidc-keys: %s: updated (%d bytes)", p, len(raw))
	}
	return failed
}

// oidcKeysDir: the flag, else WITHALBUM_OIDC_KEYS_DIR. Empty = fetch keys
// directly (needs a route to Google and Apple).
func oidcKeysDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("WITHALBUM_OIDC_KEYS_DIR")
}
