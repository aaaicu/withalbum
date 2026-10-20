//go:build unix && !darwin

package harness

// ru_maxrss is in KiB on Linux.
const rssUnit = 1024
