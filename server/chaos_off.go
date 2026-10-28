//go:build !chaos

package main

import "github.com/aaaicu/withalbum/internal/httpapi"

// chaosFaults is nil in every normal build. Only a binary built with
// "-tags chaos" (scripts/chaos-macos.sh) can stop at upload fault points.
func chaosFaults() httpapi.FaultInjector { return nil }
