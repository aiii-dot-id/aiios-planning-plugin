# id.aeon.planning

[Source repository](https://github.com/aiii-dot-id/aiios-planning-plugin)

Current work material, recorded request review and bounded planning calibration
for AII OS 0.1.19 or later. The package declares native Linux x86-64 and macOS
arm64 variants. It requires `ring4.kv`, `fs.private` and `interaction.read`;
upgrading a package does not grant permissions.

## Current work and evidence

`review` is the primary operation for resuming uncertain work or inspecting
repeated failures. In 0.8.0 its default response is compact: current work,
collection coverage, retained request counts, and up to five recent non-success
or unknown outcomes with exact evidence references and result-detail routes.
Successful calls are summarized by count; host success does not prove command
success or task correctness. Missing evidence stays unknown.

**Migration from 0.7.x:** version 0.8.0 declares `id.aeon.planning.core@2`
because the default review and material response shape changes. Use
`view:"full"` for the previous aggregate response
shape (`attempts`, all collected `evidence`, `work_changes` and unmatched results).
The default compact response uses `diagnostics` and `omitted` instead. `omitted`
counts presentation omissions separately from collection `coverage`; it does not
claim those records were unavailable. `full_review_request` is a ready-to-use
route for the complete bounded lists. It is a fresh read: compare the returned
source incarnation and cutoff rather than treating it as a frozen replay.

`review` with `{}` uses the host-stamped current session and descendants whose
parentage the host can prove. An explicit `work_session_id` keeps single-session
scope unless `include_descendants:true` is requested. Counts describe retained
recorded requests, not executor admissions, calibration completions or a complete
count of task calls. `admissions` remains a compatibility alias for
`recorded_requests`. Neither compact nor full view verifies task correctness.

`plan` remains callable and discoverable for bounded calibration. `{}` or
`mode:"material"` / `mode:"status"` without `plan_id` returns the same compact
material in its existing `current_work` / `review` wrapper, without repeating
current work inside the review. `view:"full"` retains its earlier full wrapper.
These reads register no new task and write no plugin storage. Explicit plan IDs
and the other legacy modes retain their behavior and do not accept `view`.

Diagnostics preserve host-recorded `reason_code` and up to 512 runes of retained
non-success text. Long text retains its beginning and tail with an explicit
`[... middle omitted ...]` marker. Full-view successful excerpts remain limited
to 256 runes. No view parses prose into a command outcome or reconstructs output
the host did not retain. The recent diagnostic selection does not assert that an
older failure is unresolved, or that a later successful host call corrected it.

When a reusable lesson is earned, copy the relevant entries from `evidence` into
Memory store's `evidence` array unchanged. Each carries the record ID, sequence,
digest and source incarnation. Compact includes references for its shown
diagnostics, unmatched results and current work fields; use full view to inspect
successful corrections or other history. Record only what the actual call and
result establish, with applicability and uncertainty. Persistence does not prove
the lesson true.

Pass `result_detail_request` unchanged to `review` to inspect one exact result.
The detail-only response (`scope: retained_result_detail`) preserves collection
coverage and returns up to 8,192 runes of retained transcript, without repeating
aggregate lists. It binds the original work session, source incarnation and
exact record. `content_truncated`, `execution_truncated` and `omissions` disclose
limits. A mismatched source/ref refuses; missing evidence is unavailable. The
whole response remains bounded to 60 KiB.

The host's exact record ID for this review is excluded from its own report.
If unavailable, the result says so instead of guessing by tool name. Unresolved
historical child ancestry makes descendant coverage incomplete; only proven
children are included. A bounded cutoff excludes later arrivals. Current work
material folds the root session's retained fields and references, preserves
cleared values, and reports `not_retained` when no declaration remains. It never
invents a goal or takes a child's report as the parent's state. These reads make
no model call.

If a current-work or review response exceeds its size budget, call `review` with a smaller `record_limit`, such as `{"record_limit":8}`. The bounded result reports incomplete coverage rather than implying that omitted records were checked.

## Bounded calibration

The existing `plan` modes remain `begin`, `material`, `apply`, `grade`, `advise`,
`status` and `abandon`. Explicit plan IDs retain their previous behavior.
`begin` can omit `plan_id` when the host supplies a call reference; the generated
ID is returned. Meaningful family, falsifier, expected evidence, consequence,
steps and call budget declarations are still required.

The host-only `observe` subscription records calibration events. Counts refer
to host tool completions within a bound session and sequence interval; they do
not prove external effects or task correctness. Pending grades retain their
endpoint for retry after delivery catches up. Missing measurements stay
unresolved, never zero. Closed results are frozen.

`abandon` explicitly closes unfinished work as unresolved while retaining its
declaration, steps and available measurements. It creates no result, call total
or grade; retries are idempotent. Graded plans cannot be rewritten. Abandoned
records are excluded from advice. Older executables cannot decode abandonment
records, so preserve data and do not downgrade over them.

Open plans are not automatically deleted. When a public mutation needs space,
an eligible closed record is copied into the plugin's private archive and read
back before KV removal. Storage or archive failures preserve the authoritative
record. History pagination and measurement limits are explicit in the returned
coverage. Schemas in `schemas/` define all inputs and outputs.

## Tests

Run the regression suite with the SDK version pinned in `go.mod`:

```sh
go test -race ./...
```

The suite exercises registered handlers through the SDK frame transport with
controlled broker fixtures. It covers failure, evidence and storage boundaries;
it does not replace installed-host or live provider qualification.

## Build

Requires Go 1.25 or newer and the SDK version pinned in `go.mod`.

```sh
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o dist/id.aeon.planning-linux-x86_64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o dist/id.aeon.planning-macos-arm64 .
go tool aiisdk package
```

Native activation requires a platform signature. A cross-build alone does not
qualify activation. Verify a released package with
`aii plugin verify <package.aiiospkg>` before installing it.

Copyright (c) AI Identity Incorporated. Licensed under Apache-2.0; see LICENSE.
Required dependency notices are retained in THIRD_PARTY_LICENSES.

## Calling requirements

Current-work `review`, `material` and `status` reads require an existing core
work session. The host supplies the acting session; a read never creates one.
For new work, use core `work` with `action:"start"` and `description` first.
To inspect existing work without opening another session, pass that existing
`work_session_id` to `review`. A host session lookup failure is an availability
problem: resolve it before retrying, without inventing an ID or starting
replacement work.

`plan` with `mode:"begin"` registers optional bounded calibration inside the
acting session. It does not open core work. It requires `family`,
`falsifier_class`, `expected_evidence_class`, `consequence_class`,
`declared_steps` (1–256), and `budget_calls` (1–100000). `plan_id` may be omitted
only when the host supplies a call reference. Optional `session_id` asserts the
host's session; it cannot select one. IDs and family are 1–128 ASCII characters;
class labels are 1–96. Use letters, digits, or `._:-` only, without spaces or
prose. These are labels chosen for the work, not predefined class names.

For example, after core work exists, a calibration registration can use
`{"mode":"begin","family":"checks","falsifier_class":"failed","expected_evidence_class":"passed","consequence_class":"local","declared_steps":1,"budget_calls":10}`.
The input schema's `mode` description lists the required fields for apply,
grade, advise and abandon. Do not send another mode's fields. Ordinary work
review does not require begin, apply or grade.

Missing current work now reports `REVIEW_UNAVAILABLE` for default review/material/status, and `CALIBRATION_SESSION_REFUSED` for begin with a host no-session or session-lookup-failed frame. These replace a misleading argument error; malformed user fields still use `OPERATION_ARGUMENT_INVALID`. The reason distinguishes absent work from a failed host lookup. Existing valid requests, session assertions, stored plans and calibration rules are unchanged.
