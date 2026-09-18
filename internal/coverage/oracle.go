package coverage

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Result is one Check run's outcome: every violation found (empty means clean) plus the set of
// ops the driven scenarios actually reached, for reporting.
type Result struct {
	Violations []string
	ReachedOps map[string]bool
}

// OK reports whether the run found no violations.
func (r *Result) OK() bool { return len(r.Violations) == 0 }

// Check runs §22.5's parity oracle against one admin-ops.json fixture: it drives every resource
// and data source through the fake hub (driveTunnelApp, driveProxyApp, driveAgent, driveGrant,
// driveToken, driveHubSettings, and the three data source drivers), then checks the recording
// against three assertions:
//
//  1. Per-path — each resource/action's recorded op sequence equals its declared expectation.
//  2. Field coverage — every input field the contract declares for a reached op is present in
//     the union of that op's real recorded argument keys, unless the field is staged.
//  3. Totality — every op in the contract's `names` is reached, unmanaged, or staged.
//
// strict additionally runs the expiry rule (§22.5): a staged entry the fixture has already
// caught up to must be removed. That rule is intentionally asymmetric with the rest of the
// check — it is meant to run only in this repository's own CI against its own pinned fixture,
// never from the hub's invocation, which is why it is a separate parameter rather than always-on.
func Check(contractPath, stagedPath string, strict bool) (*Result, error) {
	contract, err := LoadContract(contractPath)
	if err != nil {
		return nil, err
	}
	staged, err := LoadStaged(stagedPath)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	total, fatal := allDrivers(ctx, contract, staged)
	if len(fatal) > 0 {
		msgs := make([]string, len(fatal))
		for i, e := range fatal {
			msgs[i] = e.Error()
		}
		return nil, fmt.Errorf("the oracle's own scenarios failed to run (a bug in this package, not a coverage finding):\n%s", strings.Join(msgs, "\n"))
	}

	result := &Result{ReachedOps: map[string]bool{}}

	// Contract-schema validation failures the fake observed while recording — these are
	// themselves coverage findings (a field the resource sends that the contract does not
	// declare, or an op the contract does not know at all), reported alongside the three
	// assertions below rather than treated as harness bugs.
	for _, e := range total.ValidationErrs {
		result.Violations = append(result.Violations, "contract validation: "+e.Error())
	}

	// Assertion 1: per-path.
	for _, exp := range total.Expectations {
		if !reflect.DeepEqual(exp.WantOps, exp.GotOps) {
			result.Violations = append(result.Violations, fmt.Sprintf(
				"per-path: %s called %v, want %v", exp.Path, exp.GotOps, exp.WantOps))
		}
	}

	// Reached-op field union, from the real recorded calls — never hand-declared.
	reachedFields := map[string]map[string]bool{}
	for _, call := range total.Calls {
		result.ReachedOps[call.Op] = true
		fields := reachedFields[call.Op]
		if fields == nil {
			fields = map[string]bool{}
			reachedFields[call.Op] = fields
		}
		for k := range call.Args {
			fields[k] = true
		}
	}

	// Assertion 2: field coverage, for every op the scenarios reached.
	for op, fields := range reachedFields {
		if unmanagedReasons[op] != "" || staged[op] {
			continue
		}
		node, known := contract.InputSchemas[op]
		if !known {
			continue // already reported as a contract-validation violation above
		}
		propNames := make([]string, 0, len(node.Properties))
		for name := range node.Properties {
			propNames = append(propNames, name)
		}
		sort.Strings(propNames)
		for _, name := range propNames {
			if fields[name] {
				continue
			}
			if staged[op+"."+name] {
				continue
			}
			result.Violations = append(result.Violations, fmt.Sprintf(
				"field coverage: %s.%s is declared in the contract but no recorded call sent it — "+
					"map it to a provider attribute, or add an `unmanaged` row with a reason", op, name))
		}
	}

	// Assertion 3: totality.
	for _, op := range contract.Names {
		if result.ReachedOps[op] || unmanagedReasons[op] != "" || staged[op] {
			continue
		}
		result.Violations = append(result.Violations, fmt.Sprintf(
			"totality: %s is in the contract but no provider path reaches it — "+
				"map it to a provider attribute, or add an `unmanaged` row with a reason", op))
	}

	if strict {
		result.Violations = append(result.Violations, expiryViolations(contract, staged)...)
	}

	sort.Strings(result.Violations)
	return result, nil
}

// expiryViolations implements §22.5's cleanup rule: "a staged entry the fixture has caught up
// to must be removed." It runs only when Check is called with strict=true — this repository's
// own CI against its own pinned coverage/admin-ops.json — never against an externally supplied
// fixture, which is what breaks the two-repository landing deadlock (§22.5, "Gating here,
// without a deadlock"): the hub can land its fixture and implementation while this entry is
// still declared, because the hub's invocation never runs this rule.
func expiryViolations(contract *Contract, staged Staged) []string {
	var out []string
	for entry := range staged {
		op, field, hasField := strings.Cut(entry, ".")
		if !hasField {
			for _, name := range contract.Names {
				if name == op {
					out = append(out, fmt.Sprintf(
						"expiry: staged entry %q has been caught up to by the pinned fixture — remove it from coverage/staged.json", entry))
					break
				}
			}
			continue
		}
		if node, known := contract.InputSchemas[op]; known {
			if _, hasProp := node.Properties[field]; hasProp {
				out = append(out, fmt.Sprintf(
					"expiry: staged entry %q has been caught up to by the pinned fixture — remove it from coverage/staged.json", entry))
			}
		}
	}
	return out
}
