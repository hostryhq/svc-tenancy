// Package contractenv prepares the shared contract suite (api-specs/contracttest) for this service. It is
// used by the contract tests of the double and of the real service, so both run the same checks.
//
// The suite is stateless and example-driven: it sends each operation once with the spec's example
// inputs. This package supplies (1) the spec files, (2) the operations that cannot run that way and why,
// and (3) the example world the examples refer to (internal/seed).
package contractenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Example GSTINs in the spec carry a wrong check character; the service validates it, as the same spec
// requires. SpecPath rewrites them to valid ones in a temporary copy (spec problem reported to api-specs).
var gstinFixes = map[string]string{
	"33ABCDE1234F1Z5": "33ABCDE1234F1Z7",
	"33AAAAA0000A1Z5": "33AAAAA0000A1Z9",
}

// SpecPath returns the path of a patched copy of the tenancy OpenAPI document (with common/ beside it).
// The api-specs checkout comes from API_SPECS_DIR, or from the module cache entry this repo pins.
func SpecPath(t testing.TB) string {
	t.Helper()
	root := os.Getenv("API_SPECS_DIR")
	if root == "" {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/hostryhq/api-specs").Output()
		if err != nil {
			t.Fatalf("locate api-specs: %v", err)
		}
		root = strings.TrimSpace(string(out))
	}
	dst := t.TempDir()
	for _, d := range []string{"openapi/tenancy/v1", "common"} {
		if err := os.MkdirAll(filepath.Join(dst, d), 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(root, d))
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(root, d, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			for from, to := range gstinFixes {
				s = strings.ReplaceAll(s, from, to)
			}
			if err := os.WriteFile(filepath.Join(dst, d, e.Name()), []byte(s), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return filepath.Join(dst, "openapi/tenancy/v1/openapi.yaml")
}

// OperationIDs lists the operations of the spec file.
func OperationIDs(t testing.TB, specPath string) []string {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, item := range doc.Paths {
		for _, op := range item {
			if op.OperationID != "" {
				out = append(out, op.OperationID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// LinkOps are the link control-plane operations that the stateless example suite cannot drive: the
// examples show one link id in six different states (and one invitation code), so no single seeded state
// satisfies them. The lifecycle scenario suite (internal/linkscenario) covers each of them instead.
var LinkOps = []string{"proposeLink", "getLink", "acceptLink", "confirmLink", "declineLink", "suspendLink", "resumeLink",
	"revokeLink", "proposeLinkAmendment", "decideLinkAmendment", "removeLinkGrant", "listLinkActivity", "getLinkDecision"}

const linkReason = "link examples describe one link in several states; covered by the lifecycle scenario suite"

// SkipExampleWorld is the skip map for the run over the example world: all operations except the link
// lifecycle (see LinkOps) and createOrganisation, whose example GSTIN is already held by a verified
// registration of the example world (it runs in its own pass over an empty platform).
func SkipExampleWorld() map[string]string {
	m := map[string]string{"createOrganisation": "runs in its own pass over an empty platform: its example GSTIN is verified in the example world"}
	for _, op := range LinkOps {
		m[op] = linkReason
	}
	return m
}

// SkipAllExcept skips every operation except keep.
func SkipAllExcept(t testing.TB, specPath string, keep ...string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, op := range OperationIDs(t, specPath) {
		m[op] = "covered by another pass of the suite"
	}
	for _, k := range keep {
		delete(m, k)
	}
	return m
}
