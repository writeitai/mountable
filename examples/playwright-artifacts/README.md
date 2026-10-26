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
