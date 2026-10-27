# Keep a Playwright screenshot and trace after the worker exits

A headless Chromium run opens a local synthetic page, saves a screenshot and
a trace, and uploads both closed files with `mountable-sdk`. A later process
downloads them and checks every byte.

The browser opens only `fixture.html` in this directory. It does not visit
another site. The trace can contain the text of the page it opened. This
page is a fixture we wrote. It has no credentials.

This community example is maintained by mountable.io, independently of
Playwright. It is not a Playwright plugin, and Playwright has not endorsed
it. The hosted mountable.io service is paid. The example and client are open
source.

Pinned versions: `playwright` 1.64.1 and `mountable-sdk` 0.3.0.

## Setup

Use Node.js 20 or newer. Set `MOUNTABLE_API_KEY` in your private environment
or secret manager. Use an existing mountable.io account with a saved payment
method, and a filesystem the key can access. The example creates no account,
filesystem or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=playwright-01-artifacts).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/playwright-artifacts
npm ci
npx playwright install chromium
```

Your backend chooses the filesystem and the run id. Do not take either from
a page. Use a new run id for each capture. The run id is 1 to 40 lowercase
letters, digits or hyphens, and it must start with a letter or digit.

## Capture

`capture` opens the local fixture, stops tracing, closes the browser, then
uploads `playwright/<run>/screenshot.png` and `playwright/<run>/trace.zip`.
Each file has to be at most 4 MiB. The write uses `If-None-Match: *`, so an
existing file is left as it is.

```sh
node artifacts.mjs capture \
  --filesystem "$FS_ID" \
  --run "$RUN" \
  --dir ./out
```

The command writes `screenshot.png` and `trace.zip` into `./out` and prints
each remote path, its SHA-256 and its size. It does not print the API key.

A `402` with `filesystem_spending_cap_reached` stops that user's billable
mountable.io work. Other filesystems keep working. After a timeout, read the
path before writing it again.

## Check from a fresh process

`check` is a new process. It opens the filesystem read-only, downloads both
files, and compares them with the files in `--dir`.

```sh
node artifacts.mjs check \
  --filesystem "$FS_ID" \
  --run "$RUN" \
  --dir ./out
```

The command prints `match` and the size of each file when every byte matches.

## Clean up

```sh
node artifacts.mjs cleanup \
  --filesystem "$FS_ID" \
  --run "$RUN"
```

This deletes both files, the run directory, and `playwright/` when that
directory is empty. A file that is already gone is left gone.

## Tests

```sh
npm test
```

The tests run headless Chromium against the local fixture and a fake files
API. They check that a second process reads the same bytes, that a second
capture does not replace them, and that cleanup removes them. They make no
paid cloud calls.

Sources: [Playwright tracing](https://playwright.dev/docs/api/class-tracing),
[mountable.io Files API](https://mountable.io/docs/files-api?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=playwright-01-artifacts).

## Retain a failed Playwright Test run

This separate scenario uses **`@playwright/test` 1.64.1**. One test opens the
same local fixture and deliberately expects the wrong heading. Its config
uses `trace: { mode: "retain-on-failure", sources: false }`, no retries,
one worker, and a JSON reporter. No HTML report or viewer bundle is uploaded.
The child runner receives no storage key and blocks HTTP(S) page requests.

`failure.mjs run` waits for the runner to exit. It checks the report for the
intended assertion failure, validates the finished trace, and refuses more
than **4 MiB combined** before uploading either file. It writes only
`playwright-test/<run>/trace.zip` and `report.json`, with `ifNoneMatch: true`.
Use a new run id **and a local output directory that does not exist**.

```sh
status=0
node failure.mjs run --filesystem "$FS_ID" --run "$RUN" --dir ./failed-out || status=$?
printf 'runner status: %s\n' "$status"
# At the end of a CI job: exit "$status"
```

Expected: `retained runner_exit=1`, followed by each path, byte count and
SHA-256; the command still exits **1**. A successful evidence upload cannot
turn the failing test green. The saved code is the runner code, captured
before uploading. An upload error prints `retention_failed runner_exit=1`
and still exits 1. A setup problem, unexpected passing test or incomplete
report is also nonzero and never reports retention.

Uploads are two separate writes. A refusal or timeout may leave one file or
an unknown result. Inspect both paths before retrying; do not blindly rerun
an upload over an earlier run. Local evidence stays in `--dir`. A setup failure says `nothing uploaded`;
inspect `--dir/report.json` and run `npx playwright install chromium` if the
browser is missing.

A new process opens the filesystem **read-only**, compares both remote files
with the local originals, and saves the downloads under `./failed-out/downloaded`:

```sh
node failure.mjs check --filesystem "$FS_ID" --run "$RUN" --dir ./failed-out
npx playwright show-trace ./failed-out/downloaded/trace.zip
node failure.mjs cleanup --filesystem "$FS_ID" --run "$RUN"
```

`check` exits zero only when both files match. `cleanup` deletes only those
two remote files and their empty directories; it leaves local evidence alone.
SDK token sessions expire within an hour. The read-only handle restricts its
session; give a reviewer a separately scoped key rather than your writer key.

This is one synthetic failure, not a general test-suite uploader. Traces and
JSON failures can contain page text, source snippets, paths and request data.
Review capture and redaction before adapting it to real pages or credentials.
No API key, remote page, model, account or filesystem is created here.

The hosted service uses normal paid storage, download and entry-count rates.
At the 4 MiB payload limit, logical file content kept for a full month is
$0.00078125 or €0.0006640625; one full download adds $0.000078125 or
€0.00006640625. Up to four new entries add $0.000002 or €0.0000017 per
full month. Uploads and individual operations have no separate fee. Actual
retained bytes and storage duration determine the bill; deleted data can
remain billed until reclamation. Taxes and account-level invoice rounding
apply. Existing caps and quotas still apply; there is no free allowance.

`npm test` also runs this actual failing runner against a fake API. It checks
exit-code preservation, fresh read-only retrieval, duplicate refusal,
changed/missing files, cap/service failures and the combined size limit.

Sources: [Playwright trace options](https://playwright.dev/docs/test-use-options#recording-options),
[JSON reporter](https://playwright.dev/docs/test-reporters#json-reporter).
