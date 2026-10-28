//go:build chaos

package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aaaicu/withalbum/internal/httpapi"
)

// This file is compiled only with "-tags chaos", for the chaos test
// (scripts/chaos-macos.sh, tools/chaos). Release builds never contain it.
//
// WITHALBUM_CHAOS_FAULT=<point> makes every upload that reaches that fault
// point (httpapi.Fault*) print "CHAOS: at <point>" and block, so the test can
// SIGKILL the process exactly there.

type chaosBlock string

func (c chaosBlock) Fault(point string) error {
	if point == string(c) {
		fmt.Fprintf(os.Stderr, "CHAOS: at %s\n", point)
		select {}
	}
	return nil
}

func chaosFaults() httpapi.FaultInjector {
	point := strings.TrimSpace(os.Getenv("WITHALBUM_CHAOS_FAULT"))
	log.Printf("WARNING: chaos test build (-tags chaos); never use this binary for real data. fault point=%q", point)
	if point == "" {
		return nil
	}
	return chaosBlock(point)
}
