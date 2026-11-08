# GitLab Ref Locking Research

Research into whether GitLab can act as the lock server for a git-backed document system, using git refs as compare-and-swap lock records. This repeats the GitHub research on GitLab.com.

- Date: 2026-11-06
- Test project: `grantcarthew/ref-test` on GitLab.com (public, project ID 88596047)
- Harness: `docs/gitlab/ref-lock-harness/`
- Companion: `docs/github/ref-locking.md` (protocol, context and design decisions)
- Status: primitive validated over git transport; liveness and rate-limit constraints found; open items listed under Open Questions

## Context

The context, decisions and alternatives for the proposed p3bot document tool are in `docs/github/ref-locking.md`. This research answers one question: can the new system point a store at a GitLab repo instead of a GitHub repo, using the same ref-based lock protocol?

- GitHub remains the chosen store. The owner raised GitLab after GitHub was locked in, to see whether the system could also support it
- The owner doesn't use GitLab. GitLab support would be for other users or other setups, not a current need
- Whether to support GitLab, and so whether to build a per-provider lock driver, is not decided
- The test account is on GitLab.com Free, and results reflect Free-plan limits

## Summary

GitLab refs work as a safe distributed lock primitive over git transport. Across 80 simultaneous races, every round had exactly one winner and the remote state always matched it. The server enforces lease expectations, atomic multi-ref pushes are all or nothing, and fencing a stale holder works.

GitLab is a weaker backend than GitHub:

- The API has no conditional ref update or delete, so renew, release, takeover and checkin must go over git transport
- The only API acquire is branch creation (`refs/heads/`), which shows locks as branches and triggers CI pipelines
- Concurrent pushes to the same project fail spuriously: 33% of non-conflicting concurrent pushes were rejected with `unable to migrate objects to permanent storage`
- A pre-receive hook occasionally declines a valid push with an empty message
- GitLab.com Free enforces a burst limit of 100 API requests per minute per user (observed), and documents the same burst for git over HTTPS and LFS
- Error messages are unreliable: a concurrent branch-create loser reports `invalid reference name 'main'`

GitLab advantages over GitHub:

- One REST call reads a custom ref and returns the commit message (lock holder JSON)
- Git over HTTPS was faster than GitHub git over SSH (1.67 s against 3.0 s p50 for a push)
- The Commits API offers a per-file optimistic check (`last_commit_id`)
- Git LFS file locks work as an alternative atomic lock server

## Environment

| Item | Value |
| --- | --- |
| Date | 2026-11-06, about 12:15 to 13:00 UTC |
| Client host | e15, EndeavourOS, Brisbane, Australia |
| Go | go1.27.1 linux/amd64 |
| Project | `grantcarthew/ref-test`, public, default branch `main` at `0a805a04` (initial commit, `README.md` only), LFS enabled, CI enabled |
| Account | `grantcarthew`, GitLab.com Free, owner access (level 50) |
| Token | Personal access token, scopes `api`, `read_repository`, `write_repository`, expiry 2026-12-06. The owner planned to revoke it after testing; revocation was not verified |
| Git transport | HTTPS, username `oauth2`, token supplied through a `GIT_ASKPASS` script |
| SSH | Not usable: `Permission denied (publickey)`; no SSH key registered on the account |

### SSH host key verification

`gitlab.com` was not in `~/.ssh/known_hosts`. The host keys from `ssh-keyscan` matched GitLab's published fingerprints (docs.gitlab.com/user/gitlab_com/) and were pinned in a test-only known-hosts file; the user's `~/.ssh/known_hosts` was not changed.

| Algorithm | SHA256 fingerprint |
| --- | --- |
| ED25519 | `SHA256:eUXGGm1YGsMAS7vkcx6JOJdOGHPem5gQp4taiCfCLB8` |
| ECDSA | `SHA256:HbW3g8zUjNSksFbqTiUWPWg2Bq1x8xdGUrliXFzSnUw` |
| RSA | `SHA256:ROQFvPThGrW4RuWLoL9tq9I9zJ42fK4XywyRtbOz/EQ` |

Authentication then failed because no key is registered, so all git traffic used HTTPS with the token.

### Credential handling

The token was written to a file readable only by the user in the session scratchpad. curl read it from a config file, and git read it through an askpass script. One probe passed it to curl with `-u`, briefly exposing it in the local process list; later probes used a config file. All local copies were deleted after testing.

## Interfaces Tested

| Interface | Operation | Conditional on current value | Verdict |
| --- | --- | --- | --- |
| `git push` without force | Create ref | Yes, the server rejects if it exists | Safe |
| `git push --force-with-lease` | Update or delete ref | Yes, enforced by the server | Safe |
| `git push --atomic` with leases | Multiple refs | Yes, all or nothing | Safe |
| `git ls-remote` over HTTPS | Read refs | Not applicable | Safe; consistent after writes |
| REST `POST /repository/branches` | Create branch | Yes (fails if it exists) | Safe for acquire, `refs/heads/` only |
| REST `DELETE /repository/branches/:name` | Delete branch | No | Unsafe |
| REST `GET /repository/commits/:ref` | Read any ref, including custom namespaces | Not applicable | Safe; returns sha and message in one call |
| REST `POST /repository/commits` with `start_sha` | Create commit | Only when creating a new branch | Not a compare-and-swap on an existing branch |
| REST `POST /repository/commits`, action `last_commit_id` | Update file | Per file | File-level optimistic check, not a ref compare-and-swap |
| GraphQL `createBranch` | Create branch | Yes (`errors: ["Branch already exists"]`) | Safe for acquire, `refs/heads/` only |
| GraphQL `branchDelete` | Delete branch | No expected-value argument | Unsafe |
| GraphQL `repository.tree(ref:)` | Read custom ref | Not applicable | Works |
| Git LFS locks API | Lock and unlock a path | Create fails if locked; unlock by lock ID | Atomic; separate from refs |

## Phase 1: Exploratory Tests

### Git transport and namespaces

| Step | Action | Result | Time |
| --- | --- | --- | --- |
| N1 | Push create `refs/locks/probe` | `[new reference]` | 1.80 s |
| N2 | `ls-remote` | Lists `refs/locks/probe` | |
| N3 | Push create again (no force) | `[rejected] (non-fast-forward)` | 0.76 s |
| N4 | Lease with wrong expected value | `[rejected] (stale info)` | 0.75 s |
| N5 | Lease with correct expected value | Forced update | 1.63 s |
| N6 | `--atomic`: content valid, lock lease stale | Content `[rejected] (atomic push failed)`, lock `[rejected] (stale info)`; nothing applied | 0.73 s |
| N7 | `--atomic`: both valid | Content updated, lock deleted | 1.82 s |

Namespace probe (N8), pushing a commit to each namespace:

| Ref | Result |
| --- | --- |
| `refs/keep-around/x` | Rejected |
| `refs/merge-requests/1/head` | Rejected |
| `refs/environments/x` | Rejected |
| `refs/pipelines/1` | Rejected |
| `refs/tmp/x` | Rejected |
| `refs/p3bot/locks/x` | Accepted |
| `refs/locks/probe` | Accepted (N1) |

GitLab reserves its internal namespaces and accepts arbitrary others.

### REST API

| Step | Action | Result | Time |
| --- | --- | --- | --- |
| B1 | Branch create `locks/probe` from `main` | 201 | 1.06 s |
| B2 | Branch create again | `400 {"message":"Branch already exists"}` | 0.41 s |
| B3 | Branch create with `ref=refs/p3bot/locks/x` | 201; the API resolves custom refs as revisions | 0.87 s |
| K1 | Commits API on existing branch, stale `start_sha` | `400 A branch called 'race-content' already exists. Switch to that branch in order to make changes` | 0.45 s |
| K2 | Commits API on existing branch, current `start_sha` | Same 400; `start_sha` applies only to new branches | 0.50 s |
| R1 | `GET /repository/commits/refs%2Fp3bot%2Flocks%2Fx` | 200, returns sha `71adcc9d`, message `{"holder":"a"}` and committed date | 0.49 s |
| R2 | Same for a missing ref | `404 Commit Not Found` | 0.39 s |
| R3 | `GET /repository/branches/race-content` | 200, commit sha | 0.50 s |
| F1 | Commits API create `f.md` | 201 | 1.00 s |
| F2 | Update `f.md` with correct `last_commit_id` | 201 | 1.40 s |
| F3 | Update `f.md` with stale `last_commit_id` | `400 The file has changed since you started editing it: f.md` | 0.49 s |

Note: the committed date returned by R1 is the date in the commit object, which the client set (`git commit-tree`). Unlike GitHub's API-created lock commits, this is not a server timestamp.

### Concurrent branch create messages

Four concurrent `POST /repository/branches` for the same name: one 201; three `400 {"message":"Failed to create branch 'locks/msg-probe': invalid reference name 'main'"}`. The source ref `main` is valid; the message is misleading. Sequential duplicates get `Branch already exists`.

### GraphQL

- Introspection (`__schema`, `__type`) returned a fixed, partial schema document whatever the query, so mutations were probed by name and validation errors
- `createBranch(input:{projectPath, name, ref})` exists. First call created the branch; the second returned `{"errors":["Branch already exists"],"branch":null}`
- `branchDelete` exists with required `projectPath` and `name`. It rejects `expectedSha`, `oldrev`, `beforeOid` and `startSha` as unknown arguments, so it has no conditional delete
- `commitCreate` exists and accepts `startSha`. It rejects `lastCommitId` (top level), `force` and `beforeOid`
- `deleteBranch`, `refUpdate`, `updateRef`, `updateRefs` and `repositoryRefUpdate` do not exist
- `project.repository.tree(ref:"refs/p3bot/locks/x").lastCommit.sha` returned the custom ref's commit. GitLab GraphQL can read custom refs, unlike GitHub's

### Git LFS locks

Endpoint: `https://gitlab.com/<project>.git/info/lfs/locks`, basic auth with `oauth2` and the token, media type `application/vnd.git-lfs+json`.

| Step | Action | Result | Time |
| --- | --- | --- | --- |
| L1 | Create lock `docs/a.md` | 201, id `3692599`, owner `grantcarthew` | 0.42 s |
| L2 | Create again (same user) | `409 already locked`, body includes the existing lock | 0.42 s |
| L3 | List | One lock | 0.39 s |
| L4 | Unlock nonexistent id | 404 (HTML page) | 0.45 s |
| L5 | Unlock correct id | 200 | 0.41 s |
| L6 | Lock again | 201, new id `3692601` | 0.46 s |
| L7 | Unlock old id `3692599` | 404 | 0.42 s |
| L8 | `POST /verify` | Lock listed under `ours` | 0.36 s |

Each acquisition gets a new ID, and unlocking by an old ID fails, so the ID works as a fencing token for release. LFS locks belong to a user, not a process, have no lease or expiry, and cannot be combined atomically with a content write.

## Phase 2: Harness Tests

### Harness design

The harness follows the GitHub one: a pool of 40 lock commits pushed once under `refs/test/pool/NN`, a channel barrier releasing all racers at once, an overlap metric (`min(end) - max(start)`), and verification after every round through both `git ls-remote` and REST `GET /repository/commits/:ref`.

Differences:

- Git racers run over HTTPS with `GIT_ASKPASS`, with no SSH
- Loser classification parses git's `[remote rejected] (<reason>)` text, so server-side reasons are reported separately
- Setup pushes (reset and delete between rounds) retry up to 5 times, because GitLab occasionally declines pushes transiently
- A combined request budget of 80 per minute covers API, LFS and git HTTPS traffic. It was added after the first 429; L1 to L4 and L10 ran before it existed
- Connection warm-up opens one connection rather than one per racer, to save rate budget

### Results overview

| Test | Description | Rounds x racers | Correct | Min overlap |
| --- | --- | --- | --- | --- |
| L1 | git push create race (`refs/locks`) | 16 x 8 (two runs) | 16/16 | 1.377 s |
| L2 | git push lease race | 16 x 8 (two runs) | 16/16 | 1.357 s |
| L3 | git release (delete) vs takeover race | 8 x 8 (two runs) | 8/8 | 1.365 s |
| L4 | Fencing sequence with `git push --atomic` | Deterministic, two runs | 4/4 checks | |
| L4b | Fencing race: atomic checkin vs takeover | 19 x 2 (9 + 10) | 19/19 | 1.402 s |
| L5 | API branch-create race | 11 x 8 (5 + 6) | 11/11 | 594 ms |
| L6 | Mixed API branch-create vs git push create | 4 x 8 | 4/4 | 615 ms |
| L7 | LFS lock create race | 6 x 8 | 6/6 | 352 ms |
| L8 | Read-after-write consistency and latency | 30 writes | 0 stale | |
| L9 | Ambiguous outcome, git push killed mid-flight | 12 + 18 | All resolved | |
| L10 | Concurrent pushes to distinct refs | 8 x 8 | 21 spurious failures, 0 unsafe | |
| CI | Pipeline triggers from lock pushes | Deterministic | Recorded | |

Totals: 80 races with exactly one winner each and the remote state matching, 0 safety anomalies. Start spread was at most 2 ms in every round.

### First run (L1 to L4)

The first run classified every git loser as "remote rejected" without the reason.

- L1 winners: 5, 5, 4, 1, 0, 4, 3, 1; 56 losers rejected by the server
- L2 winners: 4, 2, 3, 7, 1, 1, 4, 1; 55 losers rejected by the server, 1 rejected by the client (stale info)
- L3 winners: 4, 0, 2, 7 (takeover won round 4); 27 rejected by the server, 1 rejected by the client
- L4 sequence passed
- L4b winners for 9 rounds: H, H, H, T, T, H, H, T, T. Before round 10, a setup reset push failed:

```text
remote: GitLab:
 ! [remote rejected] d5cb5bf1... -> refs/locks/race-doc (pre-receive hook declined)
```

The same push, retried by hand at 22:34:08 AEST, succeeded. The harness was then changed to classify reject reasons and to retry setup pushes.

### Second run (L1 to L4)

| Test | Winners | Loser reasons |
| --- | --- | --- |
| L1 | 4, 2, 1, 4, 3, 2, 0, 2 | `reference already exists` x35, `unable to migrate objects to permanent storage` x20, `pre-receive hook declined` x1 |
| L2 | 4, 1, 7, 6, 5, 5, 5, 5 | `incorrect old value provided` x33, `unable to migrate objects to permanent storage` x23 |
| L3 | 0, 0, 2, 0 (all deletes) | `reference does not exist` x23, `unable to migrate objects to permanent storage` x5 |
| L4b | H, H, H, H, T, T, T, T, T, H | `atomic transaction failed` x4, `unable to migrate objects to permanent storage` x6 |

L4 sequence, second run:

| Step | Action | Result |
| --- | --- | --- |
| 1 | H acquires `refs/locks/race-doc` | ok |
| 2 | T takes over with lease `h` | ok |
| 3 | H atomic checkin: content lease `c1`, lock lease `h` | Rejected; content `c1`, lock `t` |
| 4 | T atomic checkin: content lease `c1`, lock lease `t` | Applied; content `cT`, lock absent |

L4b invariant held every round: content changed if and only if H won, and the lock was absent if and only if H won.

Every losing reason except `unable to migrate objects` and the hook decline is a real conflict reported by the server, confirming server-side enforcement on GitLab.

### L10 Concurrent pushes to distinct refs

8 racers each pushed a different pool commit to a different ref (`refs/locks/indep-0` to `-7`) at the same moment. There is no logical conflict, so every push should succeed.

- Failed per round: 4, 2, 3, 2, 3, 2, 2, 3
- Total: 21 of 64 failed (33%), all `unable to migrate objects to permanent storage`
- Every failed push was confirmed not applied by reading back; every successful push was confirmed applied

The commits being pushed were already on the server (from the pool), so the failure happens even when no new objects are transferred. This is a liveness problem, not a safety problem: concurrent lock operations on different documents in the same project fail often and need retry.

### L5 API branch-create race

First attempt: rounds 1 to 5 correct (winners 2, 4, 6, 5, 0), then the harness stopped on a 429 (see Rate Limits).

Second run, 6 rounds:

- Winners: 2, 5, 0, 4, 4, 1
- Losers: 36 `400 Failed to create branch 'locks/race-api': invalid reference name '<sha>'`, 5 `400 Branch already exists`, 1 transport error (status -1, request failed on the client)
- The transport error was resolved by the post-round verification: that racer had not won

### L6 Mixed API and git create

Even-numbered racers used REST branch create; odd-numbered racers pushed `refs/heads/locks/race-mix` over git.

- Winners: 4, 0, 2, 2. The API won every round
- Losers: 16 git rejected by the client (the ref already existed in the advertisement), 12 API `invalid reference name`

### L7 LFS lock race

8 racers created a lock on the same new path; a new path was used each round.

- Winners: 3, 7, 5, 6, 3, 3
- Losers: 42, all `409` with the winner's lock in the body (the run labelled these "other" because the body was truncated before `already locked`; fixed in the harness afterwards)
- Every racer saw the same lock ID each round

### L8 Consistency and latency

30 iterations of a git lease push followed immediately by a REST read and `ls-remote`:

- Stale REST reads: 0 of 30
- Stale `ls-remote` reads: 0 of 30
- After a delete, both reads immediately showed the ref absent

| Operation | n | p50 | p95 | Max |
| --- | --- | --- | --- | --- |
| REST read ref (commits API) | 30 | 383 ms | 472 ms | 552 ms |
| `git ls-remote` (HTTPS) | 30 | 760 ms | 829 ms | 5.751 s |
| `git push` with lease (HTTPS) | 30 | 1.673 s | 2.126 s | 2.191 s |
| API branch create | 8 | 866 ms | 1.752 s | 1.752 s |
| API branch delete | 8 | 870 ms | 1.716 s | 1.716 s |
| LFS lock create | 8 | 429 ms | 488 ms | 488 ms |
| LFS unlock | 8 | 435 ms | 625 ms | 625 ms |

### L9 Ambiguous outcomes

A git lease push was started and the process killed after a fixed delay. After 1.5 s the ref was read back through REST.

Run 1, 12 iterations, kill after 300, 500 or 700 ms: all 12 killed, 0 applied. The kill landed during ref discovery (about 760 ms), before git sent the update.

Run 2, 18 iterations, kill after 1.0 to 1.75 s:

| Kill after | Killed | Applied |
| --- | --- | --- |
| 1.00 s | 3 of 3 | 3 |
| 1.15 s | 3 of 3 | 3 |
| 1.30 s | 3 of 3 | 3 |
| 1.45 s | 3 of 3 | 3 |
| 1.60 s | 3 of 3 | 3 |
| 1.75 s | 0 of 3 (push finished) | 3 |

Killed pushes were applied 15 of 15 times once the update had been sent. Both outcomes occur depending on timing, so a client cannot infer the result from its own failure. Read-back resolved every case with no inconsistency.

### CI pipeline triggers

1. Commit `dd59570f` added `.gitlab-ci.yml` to `main` with one job (`rules: - when: always`). Pipeline `3036681309` ran for `main` (source `push`)
2. A lock commit was pushed to `refs/locks/ci-probe`: no pipeline
3. The same commit was pushed to `refs/heads/locks/ci-probe`: pipeline `3036682201` ran for `locks/ci-probe`
4. Commit `214e82b6` removed `.gitlab-ci.yml`; both pipelines were deleted

Custom-namespace lock refs do not trigger CI. Branch-style locks, including any acquired through the branch-create API, do trigger CI in a project with a CI config.

## Rate Limits

### Observed

During L5 the API returned 429:

```text
HTTP/2 429
retry-after: 46
ratelimit-limit: 100
ratelimit-name: burst_traffic_per_user_free_plan
ratelimit-observed: 102
ratelimit-remaining: 0
ratelimit-resettime: Fri, 06 Nov 2026 12:46:00 GMT
```

- The window reset on a minute boundary
- The limit counted all API requests, including reads
- No 429 was seen on git over HTTPS, although L1 to L4 and L10 sent several hundred git requests within a few minutes, before the budget existed. Whether the documented 100-per-minute git burst is enforced is unconfirmed

### Documented (docs.gitlab.com/user/gitlab_com/rate_limits/, retrieved 2026-11-06)

The page describes per-plan limits as proposed and not yet in effect, with brownout windows before enforcement. The observed 429 named the Free-plan burst limit, so at least that limit was active during testing, possibly as a brownout.

Per-plan sustained limits (authenticated traffic for a user):

| Plan | Sustained | Burst |
| --- | --- | --- |
| Free | 5,000 per hour | 100 per minute API; 100 per minute git over HTTPS, LFS and dependency proxy, shared; 300 per minute web |
| Premium | 15,000 per hour | 1,250 per minute |
| Ultimate | 25,000 per hour | 2,000 per minute |

- These limits apply to API requests, web requests and authenticated git over HTTPS. Each clone, fetch or push counts as one request, and so does each LFS transfer
- Git over SSH is not listed under the per-plan limits. The current-limits table gives 600 SSH operations per minute per user, project and git command
- Current limits that still apply: authenticated API 2,000 per minute per user; authenticated git HTTPS 10,000 per minute per user
- The new Premium and Ultimate limits are planned for January 2027

### Request cost per lock operation (GitLab, git transport)

| Operation | Requests |
| --- | --- |
| Inspect lock (REST commits API) | 1 |
| Acquire (git push) | 1 |
| Renew, release or takeover (git push with lease) | 1 |
| Checkin plus release (git push `--atomic`) | 1, plus a fetch if the clone is stale |
| Each spurious failure retry | 1 more |

At 100 per minute shared between git and LFS, and 5,000 per hour sustained, a Free account supports roughly 1,000 to 2,000 full checkout, edit and checkin cycles per hour, less under concurrency because of retries. This assumes the git burst is enforced as documented.

## Findings

### G1 Git transport is a safe lock primitive on GitLab

Create, lease update, lease delete and atomic multi-ref pushes are all checked on the server. All 80 races produced exactly one winner (L1 to L7). Reasons reported by the server: `reference already exists`, `incorrect old value provided`, `reference does not exist`, `atomic transaction failed`.

### G2 Fencing works with `git push --atomic`

A holder whose lease was taken over cannot commit content when its checkin includes a lease on its own lock in the same atomic push (L4, L4b).

### G3 No conditional ref API

There is no API or GraphQL operation to update or delete a ref conditionally. Renew, release, takeover and checkin must use git transport.

### G4 API acquire is branches only and triggers CI

REST and GraphQL branch create are atomic, but only in `refs/heads/`. Branch-style locks appear in the UI and run CI pipelines. Use `refs/locks/*` over git.

### G5 Concurrent pushes fail spuriously

33% of simultaneous pushes to distinct refs failed with `unable to migrate objects to permanent storage` (L10). The same error accounted for 54 of the losers in the L1 to L4b second run. The pushes were not applied, so the failure is safe but needs retry with backoff.

### G6 Transient pre-receive declines

A valid push was declined with `pre-receive hook declined` and an empty `GitLab:` message, and succeeded on retry. It was seen twice across several hundred pushes.

### G7 Ambiguous outcomes need read-back

A killed push may or may not be applied depending on how far it got (L9). As on GitHub, every failure must be resolved by reading the ref back.

### G8 Error messages are unreliable

Concurrent branch-create losers report `invalid reference name '<ref>'`, and the server's reject reasons mix real conflicts with infrastructure failures. Clients must classify outcomes by reading back, not by message.

### G9 Reads are consistent and cheap

0 of 30 stale reads (L8). One REST call (`GET /repository/commits/:ref`) returns the lock commit sha and its message, so inspecting a lock costs one request.

### G10 The Free plan rate limit is the binding constraint

The API burst limit of 100 per minute was hit by the harness (L5). The documented git burst is the same, and every lock operation is at least one request.

### G11 LFS locks are an alternative atomic lock

LFS lock create is atomic (L7), and unlock by ID fences against releasing a newer lock. But locks belong to the user account, have no expiry, cannot carry holder metadata beyond the path, and cannot be combined with a content write.

## Recommended GitLab Lock Driver

Use the protocol from `docs/github/ref-locking.md` with these substitutions:

| Operation | GitHub | GitLab |
| --- | --- | --- |
| Inspect lock | REST GET ref, then GET commit | REST `GET /repository/commits/refs%2Flocks%2F<id>` (one call) |
| Acquire | REST create commit, then REST create ref | Create the commit locally, `git push origin <sha>:refs/locks/<id>` |
| Renew or take over | REST create commit, then GraphQL `updateRefs` | Create the commit locally, `git push --force-with-lease=refs/locks/<id>:<old> origin <new>:refs/locks/<id>` |
| Release | GraphQL `updateRefs` to zero | `git push --force-with-lease=refs/locks/<id>:<mine> origin :refs/locks/<id>` |
| Checkin plus release | GraphQL `updateRefs` with two refs | `git push --atomic --force-with-lease=<branch>:<base> --force-with-lease=refs/locks/<id>:<mine> origin <new>:<branch> :refs/locks/<id>` |
| Lease start time | Committer date set by GitHub | Not set by the server: lock commits are created by the client. Use the HTTP `Date` header from a GitLab response at acquire time and record it in the lock message |

Retry policy:

- After any failure, read the ref back:
  - Our value: success
  - Expected old value: retry
  - Anything else: lost
- Retry spurious failures (`unable to migrate objects to permanent storage`, `pre-receive hook declined`, transport errors) with exponential backoff and jitter
- Honour `Retry-After` on 429
- Count every git and API request against a client-side budget below 100 per minute on Free

Suitability:

- Prompt and document stores with low write rates fit well
- A busy ticket store on GitLab Free will be constrained by rate limits and by spurious failures under concurrency

## Alternatives Considered

GitLab lock mechanisms tested and not recommended:

| Alternative | Why it lost |
| --- | --- |
| REST or GraphQL branch create as acquire | Works only in `refs/heads/`, so locks show as branches and trigger CI pipelines (G4). Losers get misleading errors (G8) |
| REST or GraphQL branch delete as release | Not conditional on the current value (G3) |
| Commits API `start_sha` as compare-and-swap | Applies only when creating a new branch (K1, K2) |
| Commits API `last_commit_id` as the lock | Per-file optimistic check on content, not a ref compare-and-swap; cannot hold a lock or fence a stale holder. It could still back optimistic prompt checkins |
| Git LFS file locks | Atomic, but owned by the user account rather than a process, no expiry or lease, no holder metadata, and not atomic with content writes (G11) |
| Git over SSH | Not tested: no SSH key registered. Might avoid per-plan rate limits (open question) |
| Git transport under `refs/locks/*` (chosen) | Server-enforced leases, atomic checkin plus release, no CI triggers (G1, G2) |

## Comparison With GitHub

| Area | GitHub | GitLab.com Free |
| --- | --- | --- |
| Custom `refs/locks/*` | Accepted | Accepted; reserved internal namespaces rejected |
| Server-side lease enforcement | Yes | Yes |
| Atomic multi-ref | GraphQL `updateRefs`, `git push --atomic` | `git push --atomic` only |
| API acquire | REST create ref, any namespace | Branch create only |
| API conditional update or delete | GraphQL `updateRefs` | None |
| Read lock and holder | 2 REST calls, about 0.7 s | 1 REST call, about 0.38 s |
| GraphQL reads of custom refs | Not visible | Visible through `tree(ref:)` |
| Lock write latency (p50) | GraphQL 0.93 s; git over SSH 3.0 s | git over HTTPS 1.67 s |
| Spurious failures under concurrency | None observed (not tested on distinct refs) | 33% on distinct refs |
| Transient hook declines | None observed | 2 seen |
| Ambiguous outcomes | Yes (GraphQL, 12/12 applied after abort) | Yes (git, applied once the update is sent) |
| Error text reliable | No (opaque GraphQL error) | No (misleading API error) |
| Lock refs trigger CI | Untested | Custom refs no; branch locks yes |
| Server-set lease timestamp | Yes (API-created commit) | No |
| Rate limit on lock traffic | 80 per minute content-generating (unconfirmed whether ref writes count) | 100 per minute API and 100 per minute git HTTPS (Free) |
| LFS lock alternative | Untested | Works |

## Open Questions

- Git over SSH: whether SSH avoids the per-plan burst limits and the spurious `unable to migrate objects` failures. Needs an SSH key registered on the GitLab account
- Git HTTPS burst enforcement: whether the documented 100-per-minute git burst is active; none was observed
- GitHub comparison for L10: run concurrent pushes to distinct refs on GitHub to confirm it has no equivalent spurious failure
- Private projects: all tests ran on a public project
- Self-managed GitLab: behaviour, limits and the object migration failure may differ by version and configuration
- Protected branches and push rules: whether protected content branches or Premium push rules interfere with atomic checkin pushes
- Project access tokens: confirm the protocol works with a project-scoped token rather than a personal token
- Retry tuning: measure how many retries and how much backoff are needed to make concurrent operations reliable under G5

## Harness

Location: `docs/gitlab/ref-lock-harness/`

| File | Contents |
| --- | --- |
| `main.go` | Configuration, HTTP, LFS and git helpers, race runner, tally and verification, tests L1 to L9, setup, cleanup |
| `independent.go` | Test L10 (concurrent pushes to distinct refs) |
| `budget.go` | Combined request budget of 80 per minute for GitLab Free |
| `go.mod` | Module `glrace`, no dependencies |
| `.gitignore` | Ignores runtime artefacts `repo/` and `state.json` |

Changes made after the runs, not reflected in the recorded output:

- LFS loser bodies are kept to 400 characters, so `409 already locked` is labelled correctly
- Concurrent branch-create losers are labelled `api: 400 invalid reference name (concurrent loser)`

L9 is currently set to the second-run configuration (18 iterations, kill after 1.0 to 1.75 s).

### Requirements

- A GitLab token with `api` and `write_repository` scopes, in a file
- An askpass script so git over HTTPS can authenticate without the token on the command line
- Push access to the target project; the harness creates and deletes refs and LFS locks there
- Go 1.27.1 or newer, as declared in `go.mod` (the code needs Go 1.22 or newer)

Example askpass script (keep it and the token file readable only by you):

```sh
#!/bin/sh
case "$1" in Username*) echo oauth2 ;; *) cat /path/to/token ;; esac
```

### Configuration

Constants at the top of `main.go`:

| Constant | Default | Purpose |
| --- | --- | --- |
| `project` | `grantcarthew/ref-test` | Project path (informational) |
| `projectID` | `88596047` | Numeric project ID for the API |
| `remote` | `https://gitlab.com/grantcarthew/ref-test.git` | Git and LFS endpoint |
| `poolN` | 40 | Number of pool commits |
| `rounds` | 8 | Rounds per race test |
| `racers` | 8 | Racers per round |

`perMin` (80) is in `budget.go`.

Environment:

| Variable | Purpose |
| --- | --- |
| `GL_TOKEN_FILE` | Path to the token file |
| `GIT_ASKPASS` | Path to the askpass script |

### Running

```bash
cd docs/gitlab/ref-lock-harness
export GL_TOKEN_FILE=/path/to/token GIT_ASKPASS=/path/to/askpass
go run . l1 l2 l3 l4        # git races and fencing
go run . l10                # concurrent pushes to distinct refs
go run . l5 l6 l7           # API, mixed and LFS races (paced)
go run . l8 l9              # consistency, latency, ambiguity
go run . cleanup            # delete test refs, branch locks and LFS locks
```

The first invocation runs setup: it initialises `repo/`, fetches `main`, creates the commit pool, pushes it under `refs/test/pool/*`, and records the pool in `state.json`. Delete `state.json` (and run `cleanup`) to start fresh or to switch to another project.

The CI trigger test was run by hand and is not in the harness. To repeat it: add a minimal `.gitlab-ci.yml` to `main`, push a lock commit to `refs/locks/<x>` and to `refs/heads/locks/<x>`, list `GET /projects/:id/pipelines`, then remove the file and delete the pipelines.

Exit codes: 0 when all checks pass, 1 when any anomaly is detected, 2 on a setup or fatal error, 3 when rate-limited.

### Test IDs

| ID | Function | Approx. requests |
| --- | --- | --- |
| l1 | `testGitCreate` | 90 |
| l2 | `testGitLease` | 90 |
| l3 | `testGitReleaseVsTakeover` | 45 |
| l4 | `testFencing` | 80 |
| l5 | `testBranchAPI` | 70 |
| l6 | `testMixed` | 45 |
| l7 | `testLFS` | 60 |
| l8 | `testConsistency` | 125 |
| l9 | `testAmbiguity` | 40 |
| l10 | `testIndependent` | 200 |
| cleanup | `cleanup` | 5 |

Warning: on GitLab Free, a full run is close to 1,000 requests, against a sustained limit of 5,000 per hour and a burst of 100 per minute. The budget paces the run to stay under the burst limit.

## References

- GitLab REST API, branches: https://docs.gitlab.com/api/branches/
- GitLab REST API, commits: https://docs.gitlab.com/api/commits/
- GitLab GraphQL API: https://docs.gitlab.com/api/graphql/
- GitLab.com rate limits: https://docs.gitlab.com/user/gitlab_com/rate_limits/
- GitLab.com SSH host keys: https://docs.gitlab.com/user/gitlab_com/
- Git LFS file locking API: https://github.com/git-lfs/git-lfs/blob/main/docs/api/locking.md
- git push `--force-with-lease` and `--atomic`: https://git-scm.com/docs/git-push
