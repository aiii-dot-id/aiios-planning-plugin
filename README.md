# id.aeon.planning

[Source repository](https://github.com/aiii-dot-id/aiios-planning-plugin)

Review of recorded work for AII OS 0.1.19 or later. The package declares native
Linux x86-64 and macOS arm64 variants. It requires only `interaction.read`;
upgrading a package does not grant permissions. The plugin writes nothing and
keeps no storage of its own: work owns plans, and the host's records are the
evidence.

## Review

`review` reads one work session from the host's interaction records. It makes
no model call, timer or write, and registers nothing.

- `{}` reviews the acting work session and the descendants whose parentage the
  host can prove.
- `work_session_id` reviews that session, including one already delivered or
  abandoned, with single-session scope unless `include_descendants:true` is
  passed. `work start` and `work deliver` print the session ID.
- With no acting session and no `work_session_id`, review refuses with
  `REVIEW_UNAVAILABLE` and says to pass the ID. A failed host session lookup is
  reported as such: resolve it before retrying, without inventing an ID or
  starting replacement work.

The default compact response carries current work (the root session's latest
retained fields, with references and clipping routes), collection `coverage`,
`counts` of retained recorded requests, and `attempts`: up to five recent
attempts that did not succeed and five that did, newest first. Each attempt
names its tool, outcome, host `reason_code`, a bounded excerpt, `request_ref`,
`result_ref` and a `result_detail_request`. `omitted` counts what the
projection left out, separately from coverage; `full_review_request` is a
fresh, bounded read of everything collected. `view:"full"` returns every
selected attempt in recorded order, the work changes, unmatched results and
`evidence`, a reference for every collected record.

Excerpts keep up to 256 runes of a successful result and up to 512 of any other;
long non-success text keeps its beginning and tail around an explicit
`[... middle omitted ...]` marker. No view parses prose into an outcome or
reconstructs output the host did not retain. Showing a recent failure does not
assert that it is unresolved, nor that a later success corrected it.

## Evidence for Memory

Every reference a review returns carries `id`, `sequence`, `sha256` and
`incarnation`, the fields Memory `store` evidence requires. Copy the references
of the attempts or work fields that earned a lesson into its `evidence` array
unchanged, up to 16. The host still checks each one against the current
record, and persistence does not prove the lesson true: record only what the
call and its result establish, with applicability and uncertainty.

## Exact detail

Pass a `result_detail_request` unchanged to read one result: up to 8,192 runes
of retained transcript, with `content_truncated`, `execution_truncated` and
`omissions` disclosing limits. Pass a `work_detail_requests` entry unchanged to
read one whole retained work field, including an explicit empty-string clear;
the field is never clipped or replaced by a newer value. Both bind the original
session, scope, source incarnation and exact record, and return only the detail
with collection coverage. A changed source refuses with `REVIEW_SOURCE_CHANGED`,
a mismatched record with `REVIEW_EVIDENCE_CONFLICT`; evidence outside the
selected work or read limits is unavailable. A reference may be passed with or
without its `incarnation`, which must equal `source_incarnation`.

## Honesty and limits

Host success does not establish command or task correctness; each compact
response says so. The host's exact record for the review call itself is
excluded; if the host cannot name it, `own_call_exclusion` says `unavailable`
rather than guessing by tool name. Old child requests without retained parentage
make descendant coverage incomplete. A bounded cutoff excludes records arriving
after the review began. Missing, lost, capped or changed evidence is disclosed,
and unknown stays unknown. Responses are bounded to 60 KiB; when one exceeds it,
call `review` with a smaller `record_limit`, such as `{"record_limit":8}`, and
read the incomplete coverage it reports.

## Migration from 0.8.x

0.9.0 declares `id.aeon.planning.core@3`.

- The calibration loop is retired: the `plan` operation and its `material`,
  `status`, `begin`, `apply`, `grade`, `advise` and `abandon` modes, the host-only
  `observe` subscription, the plan registry, its 200-plan cap, archives and
  advice records. Declare a plan, expected evidence and falsifier on core
  `work`, and read current work through `review`. The plugin no longer requests
  `ring4.kv` or `fs.private`; it neither reads nor deletes data that 0.8.x left
  there.
- Compact `diagnostics` became `attempts` and includes the most recent
  successes. Compact `evidence` and `omitted.evidence_refs` are gone, because each
  shown attempt and field carries its own references; `omitted.attempts`
  replaces `successful_attempts` and `other_diagnostics`.
- `counts.admissions`, an alias of `recorded_requests`, is gone.
- References gain `incarnation`.

## Tests

```sh
gofmt -l .
go vet ./...
go test -race -count=1 ./...
```

The suite exercises the registered handler through the SDK frame transport with
controlled broker fixtures. It does not replace installed-host or live provider
qualification. The `internal/assessment` package also holds deterministic
experimental checks of revision-bound validation evidence; that controlled
harness is not a public operation or a production verification service.

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
