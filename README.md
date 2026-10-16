# id.aeon.planning

[Source repository](https://github.com/aiii-dot-id/aiios-planning-plugin)

Current work material, recorded request review and bounded planning calibration
for AII OS 0.1.19 or later. The package declares native Linux x86-64 and macOS
arm64 variants. It requires `ring4.kv`, `fs.private` and `interaction.read`;
upgrading a package does not grant permissions.

## Current work and evidence

`plan` with `{}` reads the current host work declaration and retained interaction
evidence without registering another task or writing plugin storage. `status`
without `plan_id` has the same behavior. The host supplies the acting session
through invocation metadata, separate from public operation arguments.

`review` with `{}` reads that session and descendants whose parentage the host
can prove. An explicit `work_session_id` keeps single-session behavior unless
`include_descendants:true` is requested. Results distinguish retained recorded
requests from their recorded outcomes, include bounded result excerpts and
immutable evidence references, and disclose missing, lost, capped or changed
evidence. These counts are not executor admissions, calibration completions or
an independently complete count of task calls. `admissions` remains a compatibility
alias for `recorded_requests`.

For a clipped result excerpt, pass its `result_detail_request` unchanged to `review`. It carries the original work session, source incarnation and exact result reference. The detail-only response (`scope: retained_result_detail`) includes collection coverage and `result_detail`, without repeating aggregate counts or lists. It returns up to 8,192 runes of the retained interaction transcript verbatim, including its request preview; it does not reconstruct raw tool output. `content_truncated` identifies this detail limit, `execution_truncated` preserves the recorded tool flag, and `omissions` warns that transcript retention can omit original output even when that flag is false. The whole response still has a 60 KiB limit. A source/ref mismatch refuses; a result outside the selected work or exhausted read limits is unavailable.

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
