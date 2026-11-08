# GitHub Ref Locking Research

Research into whether GitHub can act as the lock server for a git-backed document system, using git refs as compare-and-swap lock records.

- Date: 2026-11-06
- Test repository: `grantcarthew/pentest` (public)
- Harness: `docs/github/ref-lock-harness/`
- Status: primitive validated; open items listed under Open Questions
- Companion: `docs/gitlab/ref-locking.md` (the same research on GitLab.com, with a GitHub comparison)

## Context

The research supports a proposed p3bot document tool. The tool is not yet named; "doc" was a placeholder in discussion.

### Problem

Documents were spread across tools with separate storage models. start and library store prompts as CUE modules in the CUE Central Registry. tk stores tickets locally or in a git repo, and designs were added to tk only because it was the one tool with mutable, searchable storage. No single tool owns documents.

### Decisions at the time of testing

- One agent-focused tool gives access to prompts for start (public, shareable with others), designs and other documents (agent-native), and tickets
- Everything is freely hosted
- The tool has two faces: a CLI used by agents and a Go module giving direct access to document content from Go code. start reads prompts through this module
- The CLI verb for reading content is `get` (not `show`)
- All git complexity (clone, fetch, commit, push, locking) is hidden behind the CLI and module. Callers see documents and paths, not repos
- `checkout` fetches the repo if it is not local, locks the document so no other process can lock it, and returns a local path for the agent to edit directly, like `tk get <id>`. `checkin` writes the edited document back
- Attachments must be supported
- tk remains a separate tool because documents and tickets serve different purposes. tk will cross-reference documents (designs, notes, standards, guides) and will use the same underlying modules, so tk gains from this work
- Stores may also reference external repos for easy access. This might replace the `~/.agents/context` reference collection, but that was raised as a possibility, not decided
- Stores are typed by use, and the type sets the lock policy. Prompts change rarely and need no complex locking; tickets are busy and need an exclusive single-holder lock; references may need no lock at all
- GitHub is the store. git was briefly doubted as too slow (fetch, commit, push on every access), then confirmed as the right choice. The lock therefore had to be built on GitHub itself, without a separate service
- After this research, the owner asked whether a store could point at a GitLab repo instead. The owner doesn't use GitLab, so this is about possible support, not a need of their own. Results are in `docs/gitlab/ref-locking.md`. Whether the tool supports providers other than GitHub is not decided
- Confidence bar: the lock is shared by start, the document tool, tk and possibly other tools, so it had to be shown beyond doubt to work before anything is built on it

Store types. The lock policy column reflects the owner's direction; the Sync and Writes columns are proposals from discussion, not decisions:

| Store type | Lock policy | Sync | Writes |
| --- | --- | --- | --- |
| prompt | None, optimistic at checkin | Long TTL, pinned to tags for consumers | Rare, through release tags |
| document | Exclusive, with lease | Medium TTL | Occasional |
| ticket | Exclusive, short lease | Short TTL | Frequent |
| reference | None (read-only) | Pinned ref, fetched on demand | Never |

Assumption: the lock must hold across machines, not only between processes on one host. Choosing GitHub as the lock server, rather than a local lock, relies on this. It was never stated outright.

The research question: can GitHub provide a lock that is atomic, works across machines, and is fast enough, with no separate service?

## Summary

GitHub refs work as a distributed lock primitive. Across 53 simultaneous races with 392 contended operations, every round had exactly one winner and the remote state always matched it. Every path (REST, GraphQL, and git transport) goes through one server-side arbiter, the server enforces the expected old value, and multi-ref updates are atomic. That gives atomic checkin plus release, and fencing against stale lock holders.

Findings that shape the design:

- A request that fails on the client can still have been applied on the server (12 of 12 aborted requests were applied). Every failure has to be resolved by reading the ref back
- GraphQL `updateRefs` failures are opaque: a compare-and-swap conflict and a server fault return the same "Something went wrong" message
- GraphQL can write refs under `refs/locks/*` but cannot read them. Reads must use REST or `git ls-remote`
- REST `PATCH` and `DELETE` on refs do not check the current value and must never be used for lock state
- Git transport is correct but slow, about 3 s per push

## Environment

| Item | Value |
| --- | --- |
| Date | 2026-11-06, about 11:00 to 12:00 UTC |
| Client host | e15, EndeavourOS, Brisbane, Australia |
| Go | go1.27.1 linux/amd64 |
| Auth | `gh` CLI token, scopes `admin:public_key`, `gist`, `read:org`, `repo` |
| Repo permissions | admin |
| REST API version header | `X-GitHub-Api-Version: 2022-11-28` |
| Git transport | SSH, `git@github.com:grantcarthew/pentest.git` |
| Repo state | Public, default branch `main` at `5ae5f061`, no branch protection or rulesets (verified through the API after testing: `rulesets` empty, `main` not protected) |

Note: all latency figures were measured from Brisbane. Expect lower latency from regions closer to GitHub's data centres.

## The Primitive

A git ref is a name pointing at an object ID. GitHub updates refs transactionally: an update can state the value it expects the ref to have, and the update is rejected if the current value differs. That makes a ref a compare-and-swap register.

The lock design:

- Lock record: ref `refs/locks/<doc-id>`
- Lock payload: the ref points at a commit whose message is JSON holder metadata, for example `{"holder":"agent-a","host":"e15","pid":1234,"ttl":"10m"}`
- Lock commit tree: any existing tree (the tests reused the `main` tree); the commit needs no parents
- Unlocked: the ref does not exist
- Each lock attempt uses a new commit, so the object ID uniquely identifies one holder's attempt and works as a fencing token

Why custom refs under `refs/locks/`:

- They do not appear as branches in the GitHub UI or the branches API
- They are not fetched by a default clone (default refspec is `refs/heads/*`)
- Changing them creates no commits on any branch, so content history stays clean
- They are visible to `git ls-remote`, so in a public repo the lock names and holder commits are public

Two GitHub REST API families exist and are easily confused:

| API | Endpoint | Operates on | Suitable for locks |
| --- | --- | --- | --- |
| Contents API | `/repos/{o}/{r}/contents/{path}` | Files; every write is a commit on a branch | No: lock churn becomes branch history, and concurrent writes fail with 409 when the branch moves |
| Git Data API | `/repos/{o}/{r}/git/refs`, `/git/commits`, `/git/trees`, `/git/blobs` | Refs and raw git objects | Yes, for create and read only (see below) |

## Interfaces Tested

| Interface | Operation | Conditional on current value | Verdict |
| --- | --- | --- | --- |
| REST `POST /git/refs` | Create ref | Yes (fails if exists) | Safe for acquire |
| REST `PATCH /git/refs/{ref}` with `force=false` | Update ref | No, on `refs/locks/*` | Unsafe |
| REST `DELETE /git/refs/{ref}` | Delete ref | No | Unsafe |
| REST `GET /git/ref/{ref}` | Read ref | Not applicable | Safe; consistent after writes |
| GraphQL `updateRefs` with `beforeOid` | Create, update, delete; multiple refs | Yes | Safe; atomic across refs |
| GraphQL `repository.ref` / `refs(refPrefix:)` | Read ref | Not applicable | Cannot see custom namespaces |
| `git push` without force | Create ref | Yes (server rejects if exists) | Safe |
| `git push --force-with-lease=<ref>:<oid>` | Update or delete ref | Yes, enforced by the server | Safe |
| `git push --atomic` | Multiple refs | Yes, all or nothing | Safe |
| `git ls-remote` | Read refs | Not applicable | Safe; consistent after writes; slow |

## Phase 1: Exploratory Tests

Phase 1 used `gh api` and `git` by hand before the harness existed.

### REST

Setup: `main` head `5ae5f061`, tree `83973aac`. Two lock commits were created through `POST /git/commits` with JSON holder messages: A `424b7e47`, B `80aab635`.

| Step | Action | Result |
| --- | --- | --- |
| 1 | `POST /git/refs` `refs/locks/test-doc` to A | Created |
| 2 | Competing `POST /git/refs` to B | `422 Reference already exists` |
| 3 | `GET` ref and read commit message | Returned A and its holder JSON |
| 4 | Renew: child commit of A, `PATCH force=false` | Succeeded (`f4b21b29`) |
| 5 | Steal: `PATCH force=false` to unrelated commit B | Succeeded. Not a fast-forward, yet accepted |
| 6 | Steal: `PATCH force=false` to child of the old A, not the current head | Succeeded (`496710e7`) |
| 7 | Visibility | Branches API lists only `main`; `git ls-remote` shows `refs/locks/test-doc` |

Conclusion: REST create is atomic. REST `PATCH` does not enforce fast-forward on `refs/locks/*` and checks nothing, so it cannot be used for renew or release.

### Git transport

The local repo fetched `refs/locks/*`. The current lock was `496710e7`.

| Step | Action | Result | Time |
| --- | --- | --- | --- |
| G1 | `git push` create while the ref exists | Rejected (non-fast-forward) | 2.41 s |
| G2 | `--force-with-lease` with the wrong expected value | Rejected `(stale info)` | |
| G3 | `--force-with-lease` with the correct expected value | Forced update applied | 3.81 s |
| G4 | Conditional delete with the wrong expected value | Rejected `(stale info)` | |
| G5 | Conditional delete with the correct expected value | Deleted | |
| G6 | Fresh create | `[new reference]` | 3.38 s |

G2 and G4 were rejected on the client, against the advertised refs, which left server enforcement unproven at this stage. Phase 2 test T5 proved it.

### GraphQL updateRefs

Mutation used throughout:

```graphql
mutation($rid:ID!,$u:[RefUpdate!]!){
  updateRefs(input:{repositoryId:$rid,refUpdates:$u}){clientMutationId}
}
```

Each `RefUpdate` is `{name, beforeOid, afterOid, force}`. `beforeOid` of 40 zeros means the ref must not exist (create); `afterOid` of 40 zeros means delete. `force: true` was used throughout, so `beforeOid` is the only guard and fast-forward rules do not apply.

| Step | Action | Result | Time |
| --- | --- | --- | --- |
| Q1 | Update with the wrong `beforeOid` | Error "Something went wrong"; ref unchanged | |
| Q2 | Update with the correct `beforeOid` | Applied | 1.00 s |
| Q3 | Delete with the wrong `beforeOid` | Error "Something went wrong" | |
| Q4 | Delete with the correct `beforeOid` | Deleted (REST GET then 404) | 0.96 s |
| Q5 | Create (`beforeOid` zero) twice | First applied, second error | |

Timing: REST create returning 422 took 0.88 s; REST GET ref took 0.47 s.

## Phase 2: Harness Tests

### Harness design

- Language: Go, standard library only, so that it mirrors the eventual implementation
- Commit pool: 40 lock commits created locally with `git commit-tree` on the `main` tree and pushed once under `refs/test/pool/NN`. This avoids spending API write budget on commit creation and gives every racer a unique, distinguishable object ID
- Barrier: each racer is a goroutine blocked on a shared channel that is closed to release all of them at once
- Connection warm-up: before each API race, N concurrent `GET /rate_limit` calls fill the HTTP idle pool, so racers start without TLS handshakes
- Overlap metric: `overlap = min(end) - max(start)`. A positive value means every request was in flight at the same moment, so the server really had to arbitrate concurrent requests
- Verification after each round: exactly one racer reported success, and both `git ls-remote` and REST GET return that racer's commit (or absence, for delete winners)
- Loser classification: every losing response is bucketed (REST 422, GraphQL opaque error, git client-side reject, git server-side reject)
- Rate safety: a rolling budget of 60 API mutations per minute, below GitHub's documented 80, persisted in `state.json` across invocations. Any 429, or a 403 mentioning a rate limit, aborts the run
- Git racers: independent `git push` processes over SSH, with no connection multiplexing. ControlMaster was tried first and dropped because the socket path in the scratch directory (144 characters) exceeded the 108-character Unix socket limit

### Results overview

| Test | Description | Rounds x racers | Correct | Min overlap | Max start spread |
| --- | --- | --- | --- | --- | --- |
| T1 | REST create race | 8 x 8 | 8/8 | 636 ms | 1 ms |
| T2 | GraphQL compare-and-swap race (renew or takeover) | 8 x 8 | 8/8 | 745 ms | 1 ms |
| T3 | GraphQL create race (`beforeOid` zero) | 4 x 8 | 4/4 | 741 ms | 0 ms |
| T4 | Release (delete) vs takeover race | 4 x 8 | 4/4 | 755 ms | 0 ms |
| T5 | `git push --force-with-lease` race | 8 x 8 | 8/8 | 2.872 s | 1 ms |
| T6 | `git push` create race | 4 x 8 | 4/4 | 2.88 s | 1 ms |
| T7 | Mixed REST, GraphQL and git create race | 4 x 9 | 4/4 | 652 ms | 1 ms |
| T8 | Atomic multi-ref update | Deterministic | 5/5 checks | | |
| T9 | Fencing sequence | Deterministic | 2/2 checks | | |
| T9b | Fencing race: stale checkin vs takeover | 10 x 2 | 10/10 | 786 ms | 0 ms |
| T10 | Read-after-write consistency | 30 writes | 0 stale reads | | |
| T11 | Error shapes | Deterministic | Recorded | | |
| T12 | Latency distribution | See table | Recorded | | |
| T13 | Ambiguous outcome after client abort | 12 | 12 resolved by read-back | | |
| T14 | High contention, REST and GraphQL | 3 x 16 | 3/3 | 648 ms | 0 ms |
| T15 | Side effects and costs | Observational | Recorded | | |

Totals: 53 races, 392 contended operations, 0 anomalies.

### T1 REST create race

Ref `refs/locks/race-rest` was deleted before each round; 8 racers ran `POST /git/refs`, each with its own pool commit.

- Winners by round: 4, 3, 2, 1, 4, 4, 2, 7
- Overlap per round: 636 to 700 ms
- Losers: 56, all `422 Reference already exists`

Note: the run output labelled these losers "git: client-side reject" because of a string-matching order bug in the classifier ("already exists" was matched before the REST check). This was fixed before T5. The raw responses were all REST 422.

### T2 GraphQL compare-and-swap race

The ref was force-set to base commit `pool[30]`; 8 racers ran `updateRefs` with `beforeOid=base` and their own `afterOid`.

- Winners: 1, 5, 5, 2, 2, 0, 5, 3
- Overlap: 745 to 828 ms
- Losers: 56, all opaque "Something went wrong"

### T3 GraphQL create race

The ref was absent; 8 racers ran `updateRefs` with a zero `beforeOid`.

- Winners: 3, 2, 6, 5
- Losers: 28, all opaque

### T4 Release vs takeover race

The ref was set to base. Even-numbered racers deleted it (`afterOid` zero); odd-numbered racers took it over to their own commit.

- Winners: 0, 2, 0, 6. A delete won every round
- The final state matched the winner every round (ref absent)
- Losers: 28, all opaque

Note: with random arbitration, a delete winning 4 of 4 rounds has a probability of about 6%. The likely cause is that deletes resolve faster on the server. This does not affect correctness, but in a release-vs-takeover race the release tends to win.

### T5 git push lease race

The ref was set to base; 8 independent `git push --porcelain --force-with-lease=<ref>:<base> origin <own>:<ref>` processes ran.

- Winners: 0, 3, 3, 5, 1, 5, 7, 6
- Overlap: 2.872 to 3.005 s
- Losers: 56, all `[remote rejected]`, meaning the server rejected them

This proves GitHub enforces the lease expectation on the server, inside the ref transaction. Every racer saw the base value in its ref advertisement and passed the client-side check; only the server could have stopped them.

### T6 git push create race

The ref was absent; 8 racers pushed without force.

- Winners: 5, 0, 7, 7
- Losers: 28, all `[remote rejected]`

### T7 Mixed interface create race

The ref was absent; 9 racers ran, three on each path: REST create (racers 0, 3, 6), GraphQL create (1, 4, 7), and git push create (2, 5, 8).

- Winners: 0, 3, 3, 6. REST won every round
- Losers: 12 git rejects, 12 GraphQL opaque errors, 8 REST 422
- Every interface respects a win made through any other interface, so there is one shared arbiter

### T8 Atomic multi-ref update

Refs: content `refs/heads/race-content` at `c1`, lock `refs/locks/race-doc` at `l1`.

| Case | Batch | Result | State after |
| --- | --- | --- | --- |
| GraphQL, content valid, lock stale | `[content c1->c2, lock wrong->0]` | Error | content c1, lock l1 (nothing applied) |
| GraphQL, reversed order | `[lock l1->0, content wrong->c2]` | Error | content c1, lock l1 (nothing applied) |
| GraphQL, both valid | `[content c1->c2, lock l1->0]` | Applied | content c2, lock absent |
| `git push --atomic`, lock stale | Leases `content:c1`, `lock:wrong`; push `c2:content`, `:lock` | `atomic push failed for ref refs/locks/race-doc. status: 7` | content c1, lock l1 (nothing applied) |
| `git push --atomic`, both valid | Leases `content:c1`, `lock:l1` | Applied | content c2, lock absent |

Both interfaces are all-or-nothing, whatever the order of entries in the batch.

### T9 Fencing

Scenario: holder H's lease expires and T takes the lock over. H then tries to check in.

| Step | Action | Result |
| --- | --- | --- |
| 1 | H acquires (REST create lock to `h`) | ok |
| 2 | T takes over expired lease (`updateRefs` lock `h->t`) | ok |
| 3 | H checkin: `[content c1->cH, lock h->0]` | Rejected; content stays `c1`, lock stays `t` |
| 4 | T checkin: `[content c1->cT, lock t->0]` | Applied; content `cT`, lock absent |

Race variant (T9b), 10 rounds: H's checkin batch raced T's takeover from the same starting state.

- Winners: T, H, H, H, H, T, H, H, T, T (H 6, T 4)
- Invariant held every round: content changed if and only if H won, and the lock was absent if and only if H won

Because a checkin includes "lock still mine" in the same atomic batch, a holder that has lost its lease cannot write content, whatever the timing.

### T10 Read-after-write consistency

30 iterations: `updateRefs` to a new commit, then immediately a REST GET and a `git ls-remote`.

- Stale REST reads: 0 of 30
- Stale `ls-remote` reads: 0 of 30
- After a delete, both reads immediately showed the ref absent

No replica lag was observed for ref reads. The writer reads its own write immediately.

### T11 Error shapes

Responses as received, trimmed:

| Case | Response |
| --- | --- |
| GraphQL wrong `beforeOid` | `200 {"errors":[{"message":"Something went wrong while executing your query on <ts>. Please include \`<id>\` when reporting this issue."}]}` |
| GraphQL create when exists | Same opaque message |
| GraphQL `afterOid` object missing | `200 {"data":{"updateRefs":null},"errors":[{"type":"UNPROCESSABLE","path":["updateRefs"],"message":"twirp error not_found: missing"}]}` |
| GraphQL invalid ref name | `200 {"errors":[{"extensions":{...,"problems":[{"path":[0,"name"],"explanation":"Could not coerce value \"refs/locks/bad..name\" to GitRefname"}]}...` |
| REST create when exists | `422 {"message":"Reference already exists",...}` |
| REST create, object missing | `422 {"message":"Object does not exist",...}` |
| REST `PATCH force=false`, non-fast-forward on `refs/locks/*` | `200`, applied (unsafe) |
| git lease mismatch | `! <sha>:refs/locks/race-err [rejected] (stale info)` |
| git server-side reject (T5, T6) | `[remote rejected]` |
| git atomic reject | `atomic push failed for ref <ref>. status: 7` |

GraphQL returns HTTP 200 for every failure. A compare-and-swap conflict carries no `type` field and cannot be told apart from an internal fault. Validation errors (missing object, bad ref name) are typed.

### T12 Latency

| Operation | n | p50 | p95 | Max |
| --- | --- | --- | --- | --- |
| REST GET ref | 30 | 346 ms | 375 ms | 376 ms |
| REST create commit | 5 | 467 ms | 483 ms | 483 ms |
| GraphQL `updateRefs` | 30 | 933 ms | 1.047 s | 1.312 s |
| `git ls-remote` | 30 | 2.45 s | 2.539 s | 2.571 s |
| `git push --force` | 10 | 3.008 s | 3.503 s | 3.503 s |

REST create ref under contention took about 630 to 700 ms (from T1 round durations). Git operations are dominated by the SSH handshake plus the ref advertisement.

### T13 Ambiguous outcome after client abort

12 iterations: send `updateRefs`, abandon the request on the client after 150, 300 or 450 ms (well before the roughly 930 ms response), wait 1.5 s, then read back through REST.

- Aborted: 12 of 12
- Applied on the server anyway: 12 of 12
- Not applied: 0
- Inconsistencies after read-back: 0

A client-side failure (timeout, cancelled context, dropped connection) says nothing about the server outcome. A client that assumes failure would leave an orphaned lock that it holds without knowing it. Reading the ref back and comparing it to the unique commit resolves every case.

### T14 High contention

16 racers: even-numbered racers used REST create, odd-numbered racers used GraphQL create.

- Winners: 14, 14, 12 (REST each time)
- Losers: 21 REST 422, 24 GraphQL opaque
- Overlap: 648 to 702 ms

Across T7 and T14, REST create won all 7 mixed rounds. REST reaches the arbiter faster than GraphQL or git. That is not a correctness concern, but a client using GraphQL to acquire is at a systematic disadvantage against a client using REST.

### T15 Side effects and costs

- Public events feed (`GET /repos/{o}/{r}/events`): 0 events after all tests. Pushes to custom refs did not appear, though this feed can lag
- API commit created with no committer supplied: committer date `2026-11-06T11:52:08Z` against a local send time of `11:52:07Z`, so GitHub sets the date on its own clock
- Primary rate limits after all phase 2 tests: core 129 of 5000 used; GraphQL 10 of 5000 used (the GraphQL window may have reset during the run)
- About 300 API mutations ran over roughly 20 minutes at up to 60 per minute, with no secondary rate-limit response

### Harness defect found during testing

The first run of T1 to T4 reported every round as an anomaly because the check also compared a GraphQL read of the ref, which always returned absent. Winner counts, `ls-remote` and REST were correct throughout. Investigation produced finding F3. The GraphQL read was removed from the checks and T1 to T4 were re-run; the results above are from the re-run.

## Findings

### F1 One atomic arbiter

REST, GraphQL and git transport all update refs through the same server-side transaction. Exactly one of several concurrent conditional updates succeeds, whichever interface each caller uses (T1 to T7, T14).

### F2 The server enforces expected values

Git leases and GraphQL `beforeOid` are checked inside the server's ref transaction, not only on the client (T5, T6: 84 of 84 losers were `[remote rejected]`).

### F3 GraphQL cannot read custom ref namespaces

`repository.ref(qualifiedName:"refs/test/pool/00")` returns `null` and `refs(refPrefix:"refs/test/pool/")` returns `totalCount: 0`, although `updateRefs` writes those refs successfully. Lock reads must use REST `GET /git/ref/...` or `git ls-remote`.

### F4 REST PATCH and DELETE are unconditional

`PATCH` with `force=false` accepted non-fast-forward updates on `refs/locks/*`, and `DELETE` has no expected-value parameter. Neither may be used to change lock state.

### F5 Multi-ref updates are atomic

GraphQL `updateRefs` with several entries, and `git push --atomic` with leases, apply all updates or none (T8). This enables single-step checkin plus release and fencing (T9).

### F6 Client failure does not mean server failure

Abandoned requests were applied 12 of 12 times (T13). Every error, timeout or connection failure must be resolved by reading the ref back.

### F7 GraphQL compare-and-swap failures are opaque

A conflict returns HTTP 200 with a generic "Something went wrong" error and no `type`. The client cannot tell a lost race from a transient server fault without reading the ref back.

### F8 Reads are consistent straight after writes

0 of 30 stale reads through REST or `ls-remote` (T10). After a write, a read reliably reflects the latest ref state.

### F9 Arbitration is fast for REST and slow for git

REST won 7 of 7 mixed races, and deletes beat takeovers 4 of 4 (T4, T7, T14). Git transport costs about 3 s per write against about 1 s for GraphQL.

### F10 Lock refs are visible in public repos

`git ls-remote` lists `refs/locks/*`, and the holder JSON is readable from the commit. Lock metadata in a public repo is public.

## Recommended Protocol

### Operations

| Operation | Interface | Calls | Approx. latency |
| --- | --- | --- | --- |
| Inspect lock | REST `GET /git/ref/locks/<id>`, then `GET /git/commits/<sha>` for holder JSON | 2 reads | 0.7 s |
| Acquire | REST `POST /git/commits` (holder JSON), then REST `POST /git/refs` | 2 writes | 1.1 s |
| Renew | REST `POST /git/commits` (new lease), then GraphQL `updateRefs` `mine->new` | 2 writes | 1.4 s |
| Release without changes | GraphQL `updateRefs` `mine->0` | 1 write | 0.9 s |
| Take over an expired lease | REST `POST /git/commits`, then GraphQL `updateRefs` `expired->new` | 2 writes | 1.4 s |
| Checkin | Create content objects (REST blobs, tree, commit), then one GraphQL `updateRefs` `[branch head->new, lock mine->0]` | 3 or more writes | 2 s or more |
| Fallback, no API token | `git push --atomic --force-with-lease=...` for any of the above | git transport | 3 s |

### Read-back rule

After any write that does not return a clear success, including errors, timeouts and cancelled requests:

```text
current := REST GET ref
switch {
case current == attemptedAfterOid:  // our write landed
    success
case current == expectedBeforeOid:  // our write did not land
    retry (transient fault) with backoff
default:                            // someone else changed it
    lost: report holder from current commit
}
```

For a delete (`afterOid` zero), "landed" means the ref is absent. If another process deleted and re-created the ref in between, the default branch handles it because the object ID differs.

For checkin, read back both the content branch and the lock.

### Lease timing

- Lease start: the committer date GitHub assigns to the lock commit (server clock), when the commit is created through the API without a committer
- Lease length: stored in the lock commit message as a TTL
- Expiry check: compare against GitHub's clock, taken from the HTTP `Date` response header, not the local clock, to avoid client clock skew
- Holders renew before expiry. A takeover is a compare-and-swap against the expired commit, so only one taker can succeed

### Fencing

The lock commit's object ID is the fencing token. Every content write by a holder is submitted in the same atomic batch as a conditional update on its own lock. If the lease was taken over, the whole batch fails and content is untouched (T9, T9b).

Constraint: each store is a GitHub repo, and a document's lock ref must live in the same repo as the document's content. `updateRefs` and `git push --atomic` are both limited to one repo, so a lock held in a separate repo could not be fenced or released atomically with the content write.

### Store type policies

| Store type | Locking | Mechanism |
| --- | --- | --- |
| prompt | None | Optimistic checkin: the content branch update is conditional on the head the edit started from, using `beforeOid` or git non-fast-forward rejection. No lock refs, so F10 does not expose anything in the public prompt store |
| document | Exclusive lease, minutes to hours | Full protocol |
| ticket | Exclusive short lease | Full protocol; watch the write budget |
| reference | None | Read-only fetch at a pinned ref |

### Write budget per operation

Assuming every API write counts as a content-generating request, which is unverified:

| Operation | API writes |
| --- | --- |
| Acquire | 2 |
| Renew | 2 |
| Release | 1 |
| Checkin of one changed file | 4 (blob, tree, commit, `updateRefs`) |
| Full checkout, edit, checkin cycle | 6 |

At GitHub's documented 80 per minute and 500 per hour, that is roughly 80 full document cycles per hour per account. Git transport writes are not believed to count towards the API limits.

## Rate Limits Reference

Documented limits found during research (GitHub REST rate limit docs, retrieved 2026-11-06):

- Primary: 5000 requests per hour for an authenticated user
- Secondary: no more than 100 concurrent requests
- Secondary: no more than 900 points per minute for REST endpoints
- Secondary: no more than 80 content-generating requests per minute, and no more than 500 per hour
- Exceeding a limit returns 403 or 429, possibly with `Retry-After`. Continuing to send requests while limited can lead to a temporary or permanent ban of the integration or account

It is unknown whether ref creates, ref updates and Git Data API object creation count as content-generating requests.

## Alternatives Considered

| Alternative | Why it lost |
| --- | --- |
| Keep each tool's own storage (registry, tk, `~/.agents/context`) | This is the problem being solved: no owner for documents, no shared addressing, and three separate discovery mechanisms |
| Absorb tickets into the document tool | Documents and tickets serve different purposes; tk keeps its ticket workflow and shares modules instead |
| Keep prompts in the CUE registry with a publish step from the new tool | The owner chose direct access to content through the new tool's Go module |
| Lock only within one machine (SQLite or a file lock) | Doesn't hold across machines, and GitHub was chosen as the shared store |
| Hosted services for locks and metadata (Cloudflare R2 with D1 or Durable Objects, Turso) | Proposed when git looked too slow. Rejected once GitHub was locked in as the store |
| Lock files written through the Contents API | Every lock change becomes a commit on the branch, and concurrent branch writes fail with 409 |
| Lock files committed and pushed through git | The slow path that prompted the speed concern: every lock change needs a pull, commit and push |
| REST `PATCH` and `DELETE` on lock refs | Shown unsafe: no check against the current value (F4) |
| GitHub-hosted lock refs (chosen) | Atomic, consistent and fenced, with no separate service (F1, F2, F5, F8) |

## Open Questions

- Private repo behaviour: all tests ran on a public repo. Re-run the harness against a private repo, which is the intended home for documents and tickets
- Fine-grained tokens: verify the protocol works with a fine-grained personal access token limited to Contents read and write on the store repos
- Actions and webhooks: verify whether pushes to `refs/locks/*` trigger `on: push` workflows or `push` webhooks. Repos with workflows could otherwise run CI on every lock operation
- Rulesets and branch protection: verify that org or repo rulesets do not block `refs/locks/*` and do not interfere with atomic batches that include a protected branch
- Secondary rate-limit accounting: confirm whether ref and Git Data writes count towards the limit of 80 per minute and 500 per hour, especially for busy ticket stores
- Statistical confidence: 53 races cannot prove the absence of a race condition. Confidence also rests on the results matching git's transactional ref update semantics, which every interface evidently uses
- Fairness: REST and deletes win arbitration systematically. Decide whether a GraphQL-only client needs randomised backoff to avoid starvation
- Orphaned objects: every lock operation leaves an unreachable commit until GitHub's garbage collection removes it. They are tiny, but long-running busy stores should be monitored for repo size growth
- GraphQL `updateRefs` stability: confirm that the mutation is generally available and not in preview
- Provider support: whether stores can live on GitLab (or other git hosts) as well as GitHub, which would put a per-provider lock driver behind the shared `lock` module. GitLab is safe but weaker (`docs/gitlab/ref-locking.md`)
- Concurrent pushes to distinct refs: GitLab rejected 33% of non-conflicting concurrent pushes (`docs/gitlab/ref-locking.md`, L10). The equivalent test has not been run on GitHub
- Attachment storage: attachments are required, but no storage was chosen once GitHub was locked in. Options raised were git LFS (free quota limited; current figure unverified), in-repo binaries (heavier clones), and content-addressed object storage outside GitHub (conflicts with GitHub as the only store)
- Offline use: whether `get` must work without network access was asked and not answered
- Use from several machines: whether private documents are worked on from more than one machine was asked and not answered. The cross-machine lock assumption in Context depends on it
- Shared module home: whether the shared modules (store, lock, index, address) live in the new tool's repo or in a separate module imported by the new tool, tk and start
- Prompt migration: start reading prompts through the new module leaves undecided what happens to the `library/` CUE modules, the registry publishing, and the schema validation `cue vet` provides today

## Harness

Location: `docs/github/ref-lock-harness/`

| File | Contents |
| --- | --- |
| `main.go` | Configuration, HTTP, GraphQL and git helpers, race runner, tally and verification, tests T1 to T11, setup, cleanup |
| `extra.go` | Tests T13 (ambiguity), T14 (contention), T15 (side effects) |
| `go.mod` | Module `lockrace`, no dependencies |
| `.gitignore` | Ignores runtime artefacts `repo/` and `state.json` |

T12 latency is measured inside T10 (`testConsistency`).

### Requirements

- `gh` CLI authenticated with `repo` scope; the token is read through `gh auth token` and never printed
- SSH access to the target repo for git transport
- Push access to the target repo; the harness creates and deletes refs there
- Go 1.27.1 or newer, as declared in `go.mod` (the code itself needs only Go 1.22 or newer for `for range n`, so lowering the `go` directive is safe)

### Configuration

Edit the constants at the top of `main.go`:

| Constant | Default | Purpose |
| --- | --- | --- |
| `owner` | `grantcarthew` | Target repo owner |
| `repo` | `pentest` | Target repo name |
| `poolN` | 40 | Number of pool commits |
| `perMin` | 60 | Self-imposed API mutation budget per rolling minute |

`rounds` (8) and `racers` (8) are set further down, above the tests.

### Running

Run from the harness directory, so that `repo/` and `state.json` are created there:

```bash
cd docs/github/ref-lock-harness
go run . t11 t8                 # deterministic: error shapes, atomicity
go run . t1 t2 t3 t4            # API races (paced, several minutes)
go run . t5 t6 t7               # git and mixed races
go run . t9 t10                 # fencing, consistency, latency
go run . t13 t14 t15            # ambiguity, contention, side effects
go run . cleanup                # delete refs/test/*, refs/locks/*, refs/heads/race-content
```

The first invocation runs setup: it initialises `repo/`, fetches `main`, creates the commit pool, pushes it under `refs/test/pool/*`, and records the pool and repository node ID in `state.json`. Later invocations reuse that state. Delete `state.json` (and run `cleanup`) to start fresh or to switch to another target repo.

Exit codes: 0 when all checks pass, 1 when any anomaly is detected, 2 on a setup or fatal error, 3 when rate-limited.

### Test IDs

| ID | Function | API writes |
| --- | --- | --- |
| t1 | `testRestCreate` | 64 |
| t2 | `testGqlCAS` | 64 |
| t3 | `testGqlCreate` | 32 |
| t4 | `testReleaseVsTakeover` | 32 |
| t5 | `testGitLease` | 0 (git transport) |
| t6 | `testGitCreate` | 0 (git transport) |
| t7 | `testMixed` | 24 |
| t8 | `testAtomic` | 3 |
| t9 | `testFencing` | 24 |
| t10 | `testConsistency` (includes T12 latency) | 36 |
| t11 | `testErrors` | 7 |
| t13 | `testAmbiguity` | 12 |
| t14 | `testContention` | 48 |
| t15 | `testSideEffects` | 1 |
| cleanup | `cleanup` | 0 (git transport) |

Warning: a full run is about 350 API writes. Do not run everything within one hour of other heavy API use; GitHub's documented hourly secondary limit is 500.

## References

- GitHub REST API, Git references: https://docs.github.com/rest/git/refs
- GitHub REST API, Git commits: https://docs.github.com/rest/git/commits
- GitHub REST API rate limits: https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api
- GitHub GraphQL `updateRefs` mutation: https://docs.github.com/graphql/reference/mutations#updaterefs
- git push `--force-with-lease` and `--atomic`: https://git-scm.com/docs/git-push
- GitLab equivalent of this research: `docs/gitlab/ref-locking.md`
