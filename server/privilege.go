package main

import (
	"errors"
	"os"
)

// refuseRoot keeps the server from running as root (docs/THREAT_MODEL.md):
// it parses media, preview tools and requests from the internet (relay,
// funnel), and a flaw in any of them must not hand over the whole Mac or
// Linux host. The installers run it as the logged-in user (LaunchAgent,
// systemd --user). WITHALBUM_ALLOW_ROOT=1 overrides it, e.g. in a
// container whose only user is root.
func refuseRoot(euid int, getenv func(string) string) error {
	if euid != 0 || getenv("WITHALBUM_ALLOW_ROOT") == "1" {
		return nil
	}
	return errors.New("refusing to run as root: run WithAlbum Server as an ordinary user account (it only needs its storage folder); set WITHALBUM_ALLOW_ROOT=1 to override")
}

// euid is the effective user ID (-1 where there is none, e.g. Windows).
func euid() int { return os.Geteuid() }
