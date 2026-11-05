// Package archtest enforces the hexagonal dependency rules from
// ARCHITECTURE.md §1 by inspecting real package imports. A violation fails the
// build, so the architecture cannot erode silently.
package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

// rule constrains what the packages under prefix (relative to the module) may
// import from inside the module. Standard-library imports are always allowed.
type rule struct {
	prefix          string
	allowInternal   []string // module-relative prefixes
	allowThirdParty bool
}

var rules = []rule{
	{prefix: "core/domain", allowInternal: []string{"core/domain"}},
	{prefix: "core/ports", allowInternal: []string{"core/domain"}},
	{prefix: "core/application", allowInternal: []string{"core/domain", "core/ports", "core/application"}},
	{prefix: "adapters/inbound", allowInternal: []string{"core/domain", "core/ports", "core/application", "internal"}, allowThirdParty: true},
	{prefix: "adapters/outbound", allowInternal: []string{"core/domain", "core/ports", "internal"}, allowThirdParty: true},
	{prefix: "config", allowInternal: []string{"core/domain"}, allowThirdParty: true},
	{prefix: "internal/logging", allowInternal: nil, allowThirdParty: true},
	// The wire contract and its client depend on nothing in the platform, so
	// clients never pull in the Core or any adapter.
	{prefix: "internal/apitypes", allowInternal: nil},
	{prefix: "internal/apiclient", allowInternal: []string{"internal/apitypes"}},
	{prefix: "internal/testing", allowInternal: []string{"core/domain", "core/ports"}},
}

func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func isThirdParty(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return strings.Contains(first, ".")
}

// violation returns a description of why pkg may not import imp, or "" if the
// import is allowed. Both are module-relative for internal packages; imp is
// the raw import path otherwise.
func violation(module, pkg, imp string) string {
	rel, ok := strings.CutPrefix(pkg, module+"/")
	if !ok {
		return ""
	}

	// Nothing may import a main package under cmd/.
	if impRel, internal := strings.CutPrefix(imp, module+"/"); internal && hasPathPrefix(impRel, "cmd") {
		return "must not import cmd/ packages"
	}

	for _, r := range rules {
		if !hasPathPrefix(rel, r.prefix) {
			continue
		}
		if impRel, internal := strings.CutPrefix(imp, module+"/"); internal {
			// A package may always import within its own subtree.
			if hasPathPrefix(impRel, rel) {
				return ""
			}
			for _, allowed := range r.allowInternal {
				if hasPathPrefix(impRel, allowed) {
					return ""
				}
			}
			return "may not import " + impRel + " (rule for " + r.prefix + ")"
		}
		if isThirdParty(imp) && !r.allowThirdParty {
			return "may not import third-party package " + imp + " (rule for " + r.prefix + ")"
		}
		return ""
	}
	return ""
}

func TestViolationRules(t *testing.T) {
	const mod = "personal-ai"
	tests := []struct {
		name, pkg, imp string
		wantViolation  bool
	}{
		{"domain imports domain", "core/domain/model", "core/domain/compute", false},
		{"domain imports stdlib", "core/domain/model", "fmt", false},
		{"domain imports third party", "core/domain/model", "gopkg.in/yaml.v3", true},
		{"domain imports ports", "core/domain/model", "core/ports", true},
		{"domain imports adapter", "core/domain/model", "adapters/outbound/compute/runpod", true},
		{"ports import domain", "core/ports", "core/domain/model", false},
		{"ports import application", "core/ports", "core/application/scheduling", true},
		{"application imports ports", "core/application/scheduling", "core/ports", false},
		{"application imports adapter", "core/application/model", "adapters/inbound/rest", true},
		{"application imports config", "core/application/model", "config", true},
		{"application imports third party", "core/application/model", "github.com/jackc/pgx/v5", true},
		{"inbound imports application", "adapters/inbound/rest", "core/application/model", false},
		{"inbound imports outbound", "adapters/inbound/rest", "adapters/outbound/persistence/postgres", true},
		{"inbound imports other inbound", "adapters/inbound/telegram", "adapters/inbound/rest", true},
		{"inbound imports config", "adapters/inbound/rest", "config", true},
		{"outbound imports ports", "adapters/outbound/compute/runpod", "core/ports", false},
		{"outbound imports application", "adapters/outbound/compute/runpod", "core/application/deployment", true},
		{"outbound imports other outbound", "adapters/outbound/inference/vllm", "adapters/outbound/compute/runpod", true},
		{"outbound imports own subpackage", "adapters/outbound/compute/runpod", "adapters/outbound/compute/runpod/internal/api", false},
		{"config imports domain", "config", "core/domain/model", false},
		{"config imports application", "config", "core/application/scheduling", true},
		{"anything imports cmd", "adapters/inbound/rest", "cmd/server", true},
		{"cmd may import adapters", "cmd/server", "adapters/inbound/rest", false},
		{"apitypes stays stdlib only", "internal/apitypes", "core/domain/model", true},
		{"apiclient imports apitypes", "internal/apiclient", "internal/apitypes", false},
		{"apiclient imports core", "internal/apiclient", "core/application/model", true},
		{"fakes import ports", "internal/testing/fakes", "core/ports", false},
		{"fakes import adapters", "internal/testing/fakes", "adapters/outbound/persistence/postgres", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp := tt.imp
			if strings.Contains(imp, "/") && !isThirdParty(imp) || imp == "config" {
				imp = mod + "/" + imp
			}
			got := violation(mod, mod+"/"+tt.pkg, imp)
			if (got != "") != tt.wantViolation {
				t.Fatalf("violation(%s -> %s) = %q, wantViolation=%v", tt.pkg, tt.imp, got, tt.wantViolation)
			}
		})
	}
}

// TestModuleHonoursBoundaries checks every real package in the module.
func TestModuleHonoursBoundaries(t *testing.T) {
	modOut, err := exec.Command("go", "list", "-m", "-f", "{{.Path}}|{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	module, root, _ := strings.Cut(strings.TrimSpace(string(modOut)), "|")

	// Run from the module root: tests execute in their own package directory,
	// where "./..." would list only this package.
	cmd := exec.Command("go", "list", "-f", `{{.ImportPath}}|{{join .Imports ","}}`, "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./...: %v", err)
	}

	checked := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, imports, _ := strings.Cut(line, "|")
		if imports == "" {
			continue
		}
		for _, imp := range strings.Split(imports, ",") {
			checked++
			if v := violation(module, pkg, imp); v != "" {
				t.Errorf("%s imports %s: %s", pkg, imp, v)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no imports were checked; the test is not seeing the module")
	}
}
