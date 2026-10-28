package httpapi_test

import (
	"bufio"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/httpapi"
)

// specOp is one operation of docs/API_v2.yaml.
type specOp struct {
	public    bool
	responses []string
}

// readSpec reads the paths, methods, response codes and public markers of
// the OpenAPI document. The document keeps a fixed layout (2-space steps:
// path at 2, method at 4, operation keys at 6, response codes at 8), which
// is enough for this check; `ruby -ryaml` validates the YAML itself.
func readSpec(t *testing.T, path string) map[string]*specOp {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pathRE := regexp.MustCompile(`^  (/\S*):\s*$`)
	methodRE := regexp.MustCompile(`^    (get|put|post|patch|delete):\s*$`)
	codeRE := regexp.MustCompile(`^        '(\d{3})':`)
	ops := map[string]*specOp{}
	inPaths := false
	curPath := ""
	var cur *specOp
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "paths:":
			inPaths = true
			continue
		case !strings.HasPrefix(line, " ") && line != "":
			inPaths = false
		}
		if !inPaths {
			continue
		}
		if m := pathRE.FindStringSubmatch(line); m != nil {
			curPath, cur = m[1], nil
			continue
		}
		if m := methodRE.FindStringSubmatch(line); m != nil {
			key := strings.ToUpper(m[1]) + " " + curPath
			if ops[key] != nil {
				t.Fatalf("operation %s twice", key)
			}
			cur = &specOp{}
			ops[key] = cur
			continue
		}
		if cur == nil {
			continue
		}
		if line == "      security: []" {
			cur.public = true
		}
		if m := codeRE.FindStringSubmatch(line); m != nil {
			cur.responses = append(cur.responses, m[1])
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return ops
}

var paramRE = regexp.MustCompile(`\{([a-z]+)ID\}`)

// specKey converts a route pattern to "METHOD /path/{snake_id}".
func specKey(pattern string) string {
	method, path, _ := strings.Cut(pattern, " ")
	if path == "/{$}" {
		path = "/"
	}
	return method + " " + paramRE.ReplaceAllString(path, "{${1}_id}")
}

// TestOpenAPIMatchesRoutes pins docs/API_v2.yaml to the route table: the same
// operations, public exactly where the policy is public, and the statuses
// the route policy itself can return documented on each operation (401 for
// every non-public route, 403 for server routes, 404 for album routes and
// 403 for album routes whose action not every member has).
func TestOpenAPIMatchesRoutes(t *testing.T) {
	ops := readSpec(t, "../../docs/API_v2.yaml")
	seen := map[string]bool{}
	for _, rp := range httpapi.Routes() {
		if !strings.Contains(rp.Pattern, " ") {
			continue // the /api/v1/ catch-all (404 not_found / 401)
		}
		key := specKey(rp.Pattern)
		seen[key] = true
		op := ops[key]
		if op == nil {
			t.Errorf("route %s is not in API_v2.yaml (%s)", rp.Pattern, key)
			continue
		}
		if op.public != (rp.Access == httpapi.AccessPublic) {
			t.Errorf("%s: security: [] is %v, policy %s", key, op.public, rp.Access)
		}
		var want []string
		switch rp.Access {
		case httpapi.AccessSession:
			want = []string{"401"}
		case httpapi.AccessServer:
			want = []string{"401", "403"}
		case httpapi.AccessAlbum:
			want = []string{"401", "404"}
			if rp.Action != authz.GetAlbumMetadata {
				want = append(want, "403")
			}
		}
		for _, code := range want {
			if !slices.Contains(op.responses, code) {
				t.Errorf("%s (%s %s) does not document %s", key, rp.Access, rp.Action, code)
			}
		}
		if len(op.responses) == 0 {
			t.Errorf("%s has no responses", key)
		}
	}
	for key := range ops {
		if !seen[key] {
			t.Errorf("API_v2.yaml documents %s, which the server does not route", key)
		}
	}
}
