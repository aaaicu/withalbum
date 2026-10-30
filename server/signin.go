package main

import (
	"log"
	"os"
	"strings"

	"github.com/aaaicu/withalbum/internal/auth/oidc"
)

// Sign in with Google or Apple (M54). The server needs only the app's
// public client IDs to check that an ID token was issued for this app; no
// provider secret exists on the server.
//
// defaultAppleClientIDs is the app's bundle ID (native Sign in with Apple
// on iOS). defaultGoogleClientIDs is empty in source builds; release builds
// may set the app's OAuth client IDs with
// -ldflags "-X main.defaultGoogleClientIDs=<ios-client-id>,<web-client-id>".
var (
	defaultGoogleClientIDs = ""
	defaultAppleClientIDs  = "aaaicu.withalbum.app"
)

// clientIDsSetting: the flag, else the environment variable, else the
// built-in default. "off" turns the provider off.
func clientIDsSetting(flagValue, env, def string) []string {
	v := flagValue
	if v == "" {
		v = os.Getenv(env)
	}
	if v == "" {
		v = def
	}
	if strings.EqualFold(strings.TrimSpace(v), "off") {
		return nil
	}
	var out []string
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func signInConfig(googleFlag, appleFlag string) oidc.Config {
	cfg := oidc.Config{
		GoogleAudiences: clientIDsSetting(googleFlag, "WITHALBUM_GOOGLE_CLIENT_IDS", defaultGoogleClientIDs),
		AppleAudiences:  clientIDsSetting(appleFlag, "WITHALBUM_APPLE_CLIENT_IDS", defaultAppleClientIDs),
	}
	var on []string
	for p, ids := range map[string][]string{"google": cfg.GoogleAudiences, "apple": cfg.AppleAudiences} {
		if len(ids) > 0 {
			on = append(on, p)
		}
	}
	if len(on) == 0 {
		log.Printf("sign-in with Google/Apple: off (no client IDs)")
	} else {
		log.Printf("sign-in with Google/Apple: %s (linked accounts only)", strings.Join(sortedProviders(on), ", "))
	}
	return cfg
}

func sortedProviders(ps []string) []string {
	var out []string
	for _, want := range []string{"google", "apple"} {
		for _, p := range ps {
			if p == want {
				out = append(out, p)
			}
		}
	}
	return out
}
