# mdedit — design and contract

A single Go binary that lets you edit Markdown files in the browser with a
WYSIWYG editor, against directories that already exist on disk (for example
the directories a Caddy `file_server` is serving read-only). No git
awareness, no file browser, no uploads: the file host you already have is the
browser; mdedit is only the editor.

Intended to be open-sourced under MIT. Nothing host-specific (hostnames, home
layout) goes in the code; it all goes in flags and `examples/`.

## Shape

- `main.go` — flags, wiring, listener (plain TCP or tsnet).
- `internal/store` — path resolution and the read / conditional-write rules.
- `internal/server` — HTTP handlers, security middleware.
- `internal/web` — `embed.FS` of `internal/web/static/*` (built assets,
  committed so `go install` works without Node).
- `web/` — TypeScript source for the editor page, bundled with esbuild into
  `internal/web/static/`.
- `deploy/mdedit.service` — systemd user unit (installed as a symlink).
- `examples/` — Caddy snippet, `browse.html` template, env file example.

Module path: `github.com/erikus/mdedit`. Go 1.26. Constants for every default
and limit live at the top of the file that uses them; no inline magic numbers.

## Flags

| flag | default | meaning |
| --- | --- | --- |
| `--root NAME=DIR` | (required, repeatable) | Expose DIR under the URL prefix NAME. NAME matches `^[A-Za-z0-9_-]+$`. DIR is made absolute and symlink-resolved at startup. |
| `--listen ADDR` | `127.0.0.1:8585` | Plain HTTP listener. Ignored when `--tsnet-hostname` is set. |
| `--tsnet-hostname NAME` | (empty = off) | Run as its own Tailscale node named NAME using `tsnet`, serving HTTPS on :443 with Tailscale-issued certs (`ListenTLS`). Login URL is logged if the node is not yet authorized; `TS_AUTHKEY` env is honoured by tsnet. |
| `--tsnet-dir DIR` | `$XDG_STATE_HOME/mdedit/tsnet` or `~/.local/state/mdedit/tsnet` | tsnet state directory. |
| `--allowed-host HOST` | (repeatable) | Extra Host header values to accept (see Host check). |
| `--view-base URL` | (empty) | If set, the editor shows a "view" link to `URL + "/" + path`. |
| `--ext LIST` | `.md,.markdown` | Comma-separated, case-insensitive extension allowlist for editable files. |
| `--max-bytes N` | `10485760` (10 MiB) | Maximum editable file size and request body size. |
| `--max-image-bytes N` | `20971520` (20 MiB) | Maximum size served by `/img/`. |

## URL paths

A *file path* in URLs and JSON is `NAME/rel/ative/file.md`: the root name,
then a slash-separated relative path. It is never absolute, never contains
`.`/`..` segments, and is validated by the store (below).

| method + path | purpose |
| --- | --- |
| `GET /` | Tiny HTML page listing the configured root names. |
| `GET /edit/{filepath}` | The editor page (static `index.html`, same bytes for every path; the JS reads the file path from `location.pathname`). |
| `GET /api/file?path={filepath}` | Read. JSON `{ "path", "content", "version", "size", "modified", "normalized": {"crlf": bool, "bom": bool} }`. `content` is UTF-8 text with LF line endings and no BOM. `version` is the hex SHA-256 of the raw bytes on disk. |
| `PUT /api/file` | Conditional write. JSON body `{ "path", "content", "version" }`. Responds `{ "path", "version", "size" }` with the new version. |
| `GET /img/{filepath}` | Serve a file inside a root **only if** its sniffed content type (`http.DetectContentType`) starts with `image/`. Used for images referenced relatively from Markdown. Any extension allowed; same path rules; size ≤ `--max-image-bytes`. Response headers: `Content-Security-Policy: sandbox; default-src 'none'`, `X-Content-Type-Options: nosniff`, `Content-Disposition: inline`. |
| `GET /static/*` | Embedded assets. |

Error responses are JSON `{ "error": "message" }` with an appropriate status:
400 bad input, 403 forbidden (security checks), 404 missing, 409 conflict
(`{ "error", "version": "<current>" }`), 413 too large, 415 wrong type.

## Store rules (internal/store)

Resolution of a file path, in order; any failure is an error (403 for
containment violations, 400 for malformed, 404 for missing):

1. Split on `/`. First segment must be a configured root name. Remaining
   segments: at least one; none empty, `.`, or `..`; none containing NUL or
   `\`; none equal to `.git` (case-insensitive). The last segment's extension,
   lower-cased, must be in the allowlist (for `/img/`, skip the extension rule).
2. Join to `rootDir/rel...`. `filepath.EvalSymlinks` the **parent directory**;
   it must equal the resolved root or start with `resolvedRoot + separator`.
3. `Lstat` the final element. If it is a symlink, `EvalSymlinks` it and apply
   the same containment check to the target. The resolved path must be a
   regular file. Missing → 404 (mdedit edits existing files only).
4. The resolved, real path is the one used for reads and for the atomic
   replace. (Replacing the real target keeps an in-root symlink intact.)

Read: `size ≤ maxBytes`; read all bytes; `version = sha256(raw)`. Strip a UTF-8
BOM if present (`bom=true`). Reject if not valid UTF-8. Detect CRLF: if the
text contains `\r\n`, treat the file as CRLF (`crlf=true`) and convert all
`\r\n` to `\n` for `content`.

Write (`Save(path, content, expectedVersion)`), under a store-wide mutex:

1. Resolve as above; re-read current bytes; if `sha256 != expectedVersion` →
   409 with the current version.
2. Reject `content` that is not valid UTF-8 or exceeds `maxBytes`.
3. Re-apply the original file's conventions: if it was CRLF, convert `\n` to
   `\r\n`; if it had a BOM, prepend it; if the original ended with a newline
   and the new content does not, append one (and vice versa: if the original
   had no trailing newline, do not add one).
4. Write to a temp file in the same directory (`.mdedit-*`), same mode bits as
   the target, fsync, then `os.Rename` over the resolved real path. Remove the
   temp file on any failure.
5. Return the new version (sha256 of the bytes written).

## Security (internal/server)

- **Host check.** Every request's `Host` (port stripped, lower-cased) must be
  in the allowed set: the `--listen` host, `localhost`, `127.0.0.1`, `::1`,
  the tsnet node's short hostname and FQDN (from `LocalClient().Status`), and
  any `--allowed-host`. Otherwise 400 JSON. (DNS-rebinding defence.)
- **Writes need a custom header.** Any non-`GET`/`HEAD`/`OPTIONS` request must
  carry `X-Mdedit: 1`; otherwise 403. This forces a CORS preflight for any
  cross-origin caller, and mdedit never answers preflights, so cross-site
  request forgery from other origins is impossible.
- **Origin / Sec-Fetch-Site.** If an `Origin` header is present on a write it
  must match the request's own scheme+host (scheme is `https` under tsnet,
  else `http`); if `Sec-Fetch-Site` is present it must be `same-origin` or
  `none`. Otherwise 403.
- **Response headers everywhere:** `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: no-referrer`, `Cache-Control: no-store` (except
  `/static/`, which may be cached).
- **CSP on the editor page and `/`:**
  `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'self'; form-action 'none'; frame-ancestors 'none'; object-src 'none'`.
  `'unsafe-inline'` for styles only, because ProseMirror sets style
  attributes. No inline scripts anywhere.
- **Body limits.** `http.MaxBytesReader` on `PUT` at `maxBytes*2 + 64 KiB`
  (JSON escaping overhead), reject with 413.
- **Read surface = write surface.** `/api/file` reads obey the same extension
  allowlist as writes, so the daemon never discloses a file it could not
  edit. `/img/` discloses only files that sniff as images.
- No authentication: this is meant to run on a tailnet-private node (tsnet)
  or on localhost. The README says so plainly.
- The editor page must never render raw HTML from Markdown as HTML (see
  Frontend).

## Frontend (web/ → internal/web/static)

- TypeScript, bundled by esbuild (`web/build.mjs`) into
  `internal/web/static/app.js` and `app.css`, minified, ES2022, plus
  `THIRD_PARTY_LICENSES.txt` generated from the bundled packages (copy
  Simon Willison's web-edit approach). `index.html` is static and hand
  written. Built output is committed.
- Editor: Milkdown (`@milkdown/kit`: core, commonmark + GFM presets, history,
  listener). Framework-free (no React/Vue). Verify against the installed
  version's API; do not assume.
- On load: read `location.pathname`, strip the `/edit/` prefix to get the
  file path. Insert `<base href="/img/<dir of file>/">` into `<head>` **before**
  creating the editor so relative image references resolve to `/img/...`.
  All fetches use absolute paths (`/api/file`).
- `GET /api/file`, then create the editor from `content`. Immediately
  serialize the loaded doc back to Markdown; that string is the **baseline**.
  If `baseline !== content`, show a small persistent note: "Saving will
  reformat this file's Markdown." (the serializer normalizes markers etc.).
- Dirty = `serialize(doc) !== baseline`. Save is a no-op (with a brief
  "nothing to save" status) when not dirty, so an untouched file is never
  rewritten.
- Save on Ctrl/Cmd+S and a button: `PUT /api/file` with header
  `X-Mdedit: 1`, body `{ path, content, version }`. On 2xx update `version`
  and `baseline`. On 409 show a conflict banner with a "Reload" action that
  refetches and replaces the document (discarding local edits only after the
  user clicks). `beforeunload` guard while dirty or saving.
- Toolbar: file path, status (`Saved` / `Unsaved` / `Saving…` / `Conflict` /
  error text), **Save**, **Raw** toggle, and a **View** link if the server
  injected a view base (served by the page as a `<meta name="mdedit-view-base">`
  filled in by the Go handler — the only dynamic part of `index.html`;
  implement with `html/template` so it is escaped).
- Raw mode: a plain `<textarea>` containing the current Markdown. Switching
  back to WYSIWYG parses the textarea into the editor. Saving from raw mode
  saves the textarea content verbatim (no reformat).
- **Raw HTML in Markdown must not become live HTML.** Check how the Milkdown
  commonmark preset renders `html` nodes; if it inserts real markup, replace
  the node view so the source is shown as literal text. Pasted HTML is parsed
  by ProseMirror into the schema, which is fine.
- No external network requests; no fonts from CDNs; everything under
  `/static/`.
- Keep the UI text terse. Light/dark follows `prefers-color-scheme`.

## Deploy conventions

- Unit: `deploy/mdedit.service`, `ExecStart=%h/.local/bin/mdedit $MDEDIT_FLAGS`,
  `EnvironmentFile=%h/.config/mdedit/env`. Installed by `make install` as a
  symlink into `~/.config/systemd/user/`, alongside a `~/.local/bin/mdedit`
  symlink to the checkout's `bin/mdedit`, then `daemon-reload` and
  `enable --now`.
- `examples/env.example` shows a full `MDEDIT_FLAGS` line.
- `examples/Caddyfile.snippet` + `examples/browse.html` show the file_server
  integration: an "edit" link next to each `.md` entry pointing at
  `https://<node>/edit{{$.Path}}/{{.Name}}`.

## Makefile targets

`web` (npm ci + build), `build` (go build -o bin/mdedit), `test` (go test
./... and a frontend typecheck), `install` (symlink binary to
`~/.local/bin/mdedit`, symlink unit, daemon-reload, enable --now), `all` =
web build.

## Formatting-preserving save (web/src/merge.ts)

Problem: the editor serializes the whole document on save, so an untouched
file comes back with normalized bullets (`-` → `*`), rules (`---` → `***`),
re-padded tables, extra escapes, and so on. One checkbox toggle in a
382-line file rewrote 277 lines. Saves must be *stable*: bytes the user did
not touch stay byte-identical.

Approach: a structural three-way merge at save time, in the browser.

- `O` = the file as loaded (after the server's LF/BOM normalization).
- `N` = the editor's serialized Markdown (`getMarkdown()`).
- Parse both with the same remark processor the editor uses
  (`remarkCtx` from `@milkdown/kit/core`, so plugins/GFM match), keeping
  `position` offsets. Call the trees `oTree` and `nTree`.
- `merge(oNode, nNode)` returns Markdown text for `nNode`:
  1. If the nodes are structurally equal (same type, same own attributes,
     recursively equal children; `position` ignored), return the exact
     source slice of `oNode` from `O`.
  2. Recurse only into these container types: `root`, `list`, `listItem`.
     For `root`: align children by structural equality using a sequence
     diff (LCS over child hashes). Children matched to an `O` child emit the
     `O` slice; others emit their `N` slice. Between two consecutive emitted
     children that both came from `O` and were adjacent in `O`, reuse the
     original gap text; otherwise separate with `\n\n`.
     For `list` whose own attrs (`ordered`, `start`, `spread`) equal: same
     child alignment over `listItem`s. Every item emitted from `N` is
     re-markered to the `O` list's style: replace the leading
     `[-*+]` or `\d+[.)]` marker (and the task `[ ]`/`[x]` box is taken from
     `N`) with the marker style of the `O` item it replaces, or of the
     nearest `O` sibling; for ordered lists keep `O`'s numbering scheme
     (all-same-number vs incrementing). Continuation lines of an `N` item
     are re-indented from `N`'s content offset to `O`'s.
     For `listItem`: if only `checked` differs and the children are equal,
     return the `O` slice with the box text (`[ ]`/`[x]`/`[X]`) replaced.
     Otherwise prefix = `O` slice up to the first child's start (marker, box
     updated from `N`), then merge children (paragraph + nested lists) with
     the same alignment; children taken from `N` are re-indented to `O`'s
     content offset.
  3. Any other node type, or a container whose own attrs differ, is a leaf:
     equal → `O` slice, otherwise `N` slice.
- **Validation is mandatory.** After merging, parse the result and compare
  it structurally to `nTree` (positions ignored). If they differ in any way,
  discard the merge and save `N` verbatim (the current behaviour), and
  surface it in the status note. The saved file must always mean exactly
  what the editor shows; preservation is best-effort, correctness is not.
- Trailing newline, CRLF and BOM stay the server's job (unchanged).
- Also set `remarkStringifyOptionsCtx` to `{ bullet: "-", rule: "-",
  listItemIndent: "one", fences: true, emphasis: "_", strong: "*" }` so
  *new* content follows the most common style.
- The "Saving will reformat" note changes meaning: it now appears only when
  the merge fell back to `N` (validation failed), and the no-op save rule
  (`serialize(doc) === baseline` → nothing to save) is unchanged.
- Tests: a corpus under `web/test/merge/` of `(original, edited-serialized,
  expected)` triples plus property checks: `merge(O, serialize(parse(O)))`
  must equal `O` for every corpus file (stability), and `parse(merge(O, N))`
  must structurally equal `parse(N)` for every pair (correctness). Run with
  `npm test` (esbuild-bundle the tests, run with `node --test`). Do not
  commit any personal documents as fixtures.
- A small CLI, `web/scripts/merge.mjs ORIGINAL NEW`, prints the merged
  Markdown; used for ad-hoc checks and to repair files already reformatted.
