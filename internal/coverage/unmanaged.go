package coverage

// unmanagedReasons is §22.5's unmanaged-ops table, carried across verbatim rather than
// reauthored: this list is deliberately hand-written and reviewed in the spec, not generated,
// because a totality check an author can satisfy by editing a table is exactly the failure mode
// §22.5 opens by naming. An op listed here is exempt from both the totality assertion (it need
// not be reached by any driven Create/Read/Update/Delete path) and the field-coverage assertion
// (its input fields need not be recorded from a real call either).
//
// Source: docs/specs/provider/22-opentofu-provider.md, §22.5 "Unmanaged ops" table (8 rows, 10
// ops — the `app_list` row was added after this package's first draft flagged its absence; see
// that table for the row-by-row citations this map preserves one reason string per op for).
var unmanagedReasons = map[string]string{
	"admin_token_issue":  "the provider cannot mint or manage the credential it authenticates with",
	"admin_token_list":   "the provider cannot mint or manage the credential it authenticates with",
	"admin_token_revoke": "the provider cannot mint or manage the credential it authenticates with",

	"approval_list":   "runtime events, no desired state; approval_decide is refused to admin credentials",
	"approval_decide": "runtime events, no desired state; approval_decide is refused to admin credentials",

	"connection_list":   "inbound OAuth bindings are created by browser consent; revoke-only",
	"connection_revoke": "inbound OAuth bindings are created by browser consent; revoke-only",

	"app_disconnect": "clears a live oauth bundle — an imperative act, not a state",

	"audit_query": "unbounded log; see the data-source ruling",

	"app_list": "no plural apps data source: §22.4 makes pmcp_tokens \"the one plural data source, " +
		"and it earns the exception\" because its purpose is surfacing what the provider did not " +
		"create. Apps have no such blind spot — every managed app is a resource, and pmcp_app reads " +
		"one by slug — so a plural source would bake a whole inventory into state for nothing. Reads " +
		"go through app_get; agent_list is managed only because there is no agent_get",
}
