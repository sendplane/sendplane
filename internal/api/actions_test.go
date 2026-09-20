package api

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sendplane/sendplane/host"
)

// specOperation is the slice of api/openapi.yaml this test cares about.
type specOperation struct {
	OperationID string `yaml:"operationId"`
	Action      string `yaml:"x-sendplane-action"`
	// Security is a pointer so that "absent" (inherit the document default)
	// and "present but empty" (public) can be told apart, which is exactly
	// the distinction that decides whether a route needs an Action.
	Security *[]map[string][]string `yaml:"security"`
}

type specDoc struct {
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"patch": true, "head": true, "options": true, "trace": true,
}

func loadSpecOperations(t *testing.T) []specOperation {
	t.Helper()
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc specDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	var out []specOperation
	for path, item := range doc.Paths {
		for method, node := range item {
			if !httpMethods[method] {
				continue // "parameters", "summary", ...
			}
			var op specOperation
			if err := node.Decode(&op); err != nil {
				t.Fatalf("decode %s %s: %v", method, path, err)
			}
			if op.OperationID == "" {
				t.Fatalf("%s %s has no operationId", method, path)
			}
			out = append(out, op)
		}
	}
	if len(out) == 0 {
		t.Fatal("no operations found in the spec")
	}
	return out
}

// goOperationID is the identifier the generated strict server hands to a
// StrictMiddlewareFunc: the spec's operationId with an upper-case initial.
func goOperationID(id string) string { return string(id[0]-'a'+'A') + id[1:] }

// This is the compile-time table of architecture 16, checked against the spec
// it is derived from: an operation that is added to api/openapi.yaml without
// an entry here would otherwise be served with no Authorize call at all.
func TestEveryOperationHasAnAction(t *testing.T) {
	ops := loadSpecOperations(t)
	valid := map[host.Action]bool{}
	for _, a := range []host.Action{
		host.ActionSettingsRead, host.ActionSettingsWrite,
		host.ActionSenderRead, host.ActionSenderWrite,
		host.ActionTemplateRead, host.ActionTemplateWrite,
		host.ActionCampaignRead, host.ActionCampaignWrite, host.ActionCampaignSend,
		host.ActionMessageSend,
		host.ActionDeliveryRead, host.ActionDeliveryWrite,
		host.ActionSuppressionRead, host.ActionSuppressionWrite,
		host.ActionEventRead, host.ActionEventWrite,
		host.ActionProbeRun, host.ActionTrackingConfigWrite,
	} {
		valid[a] = true
	}

	seen := map[string]bool{}
	for _, op := range ops {
		id := goOperationID(op.OperationID)
		seen[id] = true
		public := op.Security != nil && len(*op.Security) == 0

		if public {
			if op.Action != "none" {
				t.Errorf("%s clears security but declares x-sendplane-action %q", id, op.Action)
			}
			if !publicOps[id] {
				t.Errorf("%s is public in the spec but missing from publicOps", id)
			}
			if _, ok := opActions[id]; ok {
				t.Errorf("%s is public in the spec but also listed in opActions", id)
			}
			continue
		}

		if publicOps[id] {
			t.Errorf("%s requires authentication in the spec but is listed in publicOps", id)
			continue
		}
		action, ok := opActions[id]
		if !ok {
			t.Errorf("%s has no entry in opActions", id)
			continue
		}
		if string(action) != op.Action {
			t.Errorf("%s: opActions says %q, the spec says %q", id, action, op.Action)
		}
		if !valid[action] {
			t.Errorf("%s maps to %q, which is not a host.Action constant", id, action)
		}
		if resourceKindFor(action) == "" {
			t.Errorf("%s maps to %q, which has no Resource.Kind", id, action)
		}
	}

	for id := range opActions {
		if !seen[id] {
			t.Errorf("opActions has %q, which is not an operation of the spec", id)
		}
	}
	for id := range publicOps {
		if !seen[id] {
			t.Errorf("publicOps has %q, which is not an operation of the spec", id)
		}
	}
}

// The table must cover every method of the generated interface too, otherwise
// an operation could be implemented and routed while the gate refuses it.
func TestActionTableCoversTheGeneratedInterface(t *testing.T) {
	total := len(opActions) + len(publicOps)
	ops := loadSpecOperations(t)
	if total != len(ops) {
		t.Fatalf("action tables cover %d operations, the spec has %d", total, len(ops))
	}
}
