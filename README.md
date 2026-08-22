# Drive

A self-hosted file store built around one hard problem: **uploading a very large file over a connection that will not stay up.**

One Go binary serves the API and the React app. File bytes never pass through it — the browser talks straight to S3-compatible object storage over presigned URLs, and the server authorizes, presigns, and keeps the ledger. An interrupted upload resumes from the last part the server confirmed, whether it was interrupted by a dropped connection, a closed tab, or the server process being killed mid-transfer.

**Live:** <https://drive.rahulsharma-cs.site> — a deployment I run and use. Sign-ups are open; verification email arrives in a few seconds. Accounts get 3 GB, single files up to 2 GB.

![The file browser with a 420 MiB upload in flight. The upload manager draws one segment per multipart part, lit as the server confirms it.](.github/media/drive.png)

## What it does

- **Resumable uploads.** A custom session protocol over S3 multipart: create a session, PUT parts directly to storage with presigned URLs, confirm each part against its MD5, complete. Resume re-handshakes and re-sends only what is missing.
- **Pause, resume, cancel, retry** per upload, from a manager that lives outside the React tree — navigating between folders never interrupts a transfer.
- **Folder drag-and-drop** (full directory trees) plus a folder picker, both normalized through the same traversal.
- **Name conflicts** resolved with keep-both / replace / skip — one prompt at a time, with an apply-to-all for the rest of the drop, so 150 colliding files can't stack 150 modals.
- **A file manager, not a list.** Rows select by click, shift-click for a run of them, cmd/ctrl-click to add one, a checkbox on the row, or a header checkbox that takes every row loaded. A row's commands are offered twice — in a kebab at the end of the row and on right-click — from one definition, so the two can't drift apart. The keyboard works as well: arrows move, Enter opens, Delete trashes the selection, Esc clears it, cmd/ctrl-A takes the page.
- **A command bar that never moves the list.** It sits above the rows at a fixed height in *every* state — nothing selected, loading, empty, failed — and crossfades between the item count and the commands instead of appearing and shoving the rows down, so the row you just clicked is still under the pointer when you reach for the next one. It offers only what the selection can actually carry out: rename needs exactly one item, download needs exactly one file (there is no archive endpoint, and a button that silently downloaded one of five would be a lie), copy is files-only because the server refuses a folder copy. On a narrow window the commands become icons and scroll sideways rather than wrapping onto a second line — wrapping would move the list, which is the thing this exists to prevent.
- **One `+ New` in the rail** for everything that adds something — new folder, upload files, upload folder — on every screen you can add something to, landing in the folder you are looking at, or in My Drive when there isn't one (search results span every folder; the trash offers no New at all). Search sits in the top bar; on a narrow window the rail folds into a drawer.
- **Move and copy**, either through a destination picker that walks one folder at a time, or by dragging rows onto a folder row or a breadcrumb ancestor. A copy is a metadata copy — it points at the same stored blob, so it costs a row, not the bytes.
- **A storage meter** in the rail that counts the same bytes the upload path counts when it decides whether to refuse — a meter that disagreed with the refusal would be worse than none.
- **Accounts** with email verification, Argon2id password hashing, durable rate limits and an Argon2 concurrency semaphore.
- **An account screen that manages the account:** change the display name; change the password, which signs out every other browser and keeps the one that submitted the form; see every session the account has open, revoke the ones you don't recognise, or sign out everywhere. A forgotten password comes back by email — a link that works once, expires in an hour, and signs every session out when it is redeemed — and an account that never verified its address can ask for that mail again. Neither route says whether an address has an account: both answer the same way either way, behind the same per-IP limit as signup and the same five-an-hour budget per address, counted separately per purpose so a stranger asking for reset links can't suppress your verification mail. One more per-IP ceiling bounds how fast a single client can ask for mail to somebody else's address.
- **Folders, trash with restore and purge, name search from the chrome** (the query lives in the URL, so a search is a location rather than a mode)**, downloads** through short-lived presigned GETs that force `Content-Disposition: attachment`, over objects deliberately stored with no content type — so even a partial response can't render as HTML.
- **Garbage collection** of unreferenced blobs and abandoned multipart uploads, on an in-process hourly ticker with a pass at startup.
- **Storage limits** that actually bind: a per-file maximum, a per-user quota, and a service-wide stored-bytes cap checked before a session is created. On the deployment above those are 2 GB, 3 GB and 8 GB — the last one is the object store's spend control, since the store itself has none.

## What it does not do

Named plainly, because a portfolio project that overstates itself is worse than a small one:

- **No sharing.** No share links, no public pages, no permissions beyond "the owner". The schema has tables for it; there are no endpoints and no UI.
- **No previews or thumbnails.** Files go in and come out as bytes.
- **No grid view, no folder item counts, no sortable columns.**
- **No mobile app, no CLI, no public API tokens.**

Sharing, then previews, are the next things worth building. Nothing above is stubbed or half-wired — it is simply not there.

## How the resume actually works

The interesting part is the failure path, so here is the sequence the browser test drives on every run:

1. The client fingerprints the file (name, size, mtime, and both edge blocks) and opens a session; the server hands back presigned part URLs.
2. Parts upload in parallel. The client compares the ETag the store returns against the MD5 it computed for that part and only then confirms it to the server, which records both. **A confirmed part is durable state in Postgres**, not browser memory.
3. The server is killed mid-transfer. The client parks the upload — *"Paused — can't reach the server. Will resume automatically."*
4. The page is reloaded, which destroys the `File` handle for good. The upload manager offers the interrupted session back and asks for **the same file**.
5. Re-picking it re-handshakes: the server merges its ledger with what the store actually holds, and the client sends only the parts neither of them has.
6. On complete, the server checks its ledger against what the store actually holds — every part present, every ETag matching — confirms the assembled object's size, and publishes it. The downloaded bytes hash equal to the source.

Step 5 is why the fingerprint matters: a different file — or the same bytes rewritten, which changes mtime — misses the match and correctly starts a fresh upload rather than silently stitching two files together.

## Measured

| What | Result |
| --- | --- |
| 2 GiB upload, Go client → local object store | 205 parts, 41 s (≈77 MiB/s) |
| 11 GiB upload | 1,127 parts, 216 s — past the 1,000-part page boundary, which is the case that breaks naive `ListParts` handling |
| Browser resume after `SIGKILL` mid-transfer | 113 MiB / 12 parts; no confirmed part re-sent; SHA-256 match |
| Production round trip (managed host + cloud object storage) | 220 MiB uploaded and downloaded, SHA-256 match, bytes never touching the app server |

The first three run on one laptop (Apple Silicon, 10 cores, 32 GB) against the local object store in Docker, so they measure the protocol and the hashing pipeline, not a network. The last one is the deployed service.

**Portability, demonstrated rather than claimed:** the same binary and the same upload protocol run against a local S3-compatible store in development and a cloud object store in production. The only difference is environment variables — endpoint, bucket, credentials, signing region.

## Running it

Needs Docker, Go 1.26, and Node 26 (`.nvmrc`).

```sh
make doctor       # preflight: docker, ports, disk, clock skew
make infra-init   # generates .env secrets, starts Postgres + object store + SMTP, creates the bucket
make dev          # API on :8080, Vite on :5173
```

`make infra-init` refuses any object-store endpoint that isn't loopback or the local compose service, so it cannot be aimed at a real bucket by accident.

Tests:

```sh
make test         # Go suite against an isolated test stack
make e2e          # Playwright
make e2e-resume   # the kill-the-server-mid-upload proof above
cd web && npx vitest run   # the web suite: the upload engine and the screens
```

The dev and test stacks are separate Docker projects on separate ports, so running tests never touches development data.

## Architecture

```
server/
  cmd/drive          the one binary: API, migrations, GC ticker, embedded SPA
  internal/api       routes, auth middleware, CSRF, rate limiting
  internal/upload    session protocol, presigning, part ledger, finalize
  internal/blob      S3 client
  internal/node      folders, files, trash, search
  internal/gc        unreferenced blobs and abandoned multipart uploads
  internal/uploadclient  importable Go client for the upload protocol
web/src/features/upload/engine   the browser upload engine
```

- **Postgres owns all metadata**, including the resume ledger. Migrations are embedded in the binary, and an applied one is never rewritten — a schema change gets a new numbered file.
- **The upload engine is a pure state machine** with clock, RNG, XHR, IndexedDB and web locks all injected, so its test suite is deterministic by construction and needs no browser.
- **The SPA is embedded in the binary** (`go:embed`), which is what lets cookies stay `SameSite=Lax` with no cross-origin credentialed CORS anywhere.

## Testing standard

A green suite was not treated as evidence. Every test that counts was shown to **fail** with the production behavior deliberately broken, then restored — because two adversarial reviews of this codebase each found real defects, including vacuous tests, in code that was 100% green.

## License

MIT. See [LICENSE](LICENSE).
