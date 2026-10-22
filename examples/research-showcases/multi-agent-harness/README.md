# One shared folder for a planner, coder and verifier

Mountable is a persistent filesystem for AI agents. This runnable example
gives a planner, a coder and a verifier the same folder. The planner writes
a plan and a failing adder. The coder fixes it. A signed file-change webhook
starts a verifier with a read-only mount, which checks the diff and runs tests.

The three stages are small, deterministic Python functions, so you can run
the whole handoff without model credentials. Use them as the storage and
permission pattern when connecting your own coding agents. This example implements the shared-workspace handoff between the three
stages. Mountable maintains it under Apache-2.0.

## Run the offline example

From the repository root, with Python 3.10 or later:

```sh
python3 examples/research-showcases/multi-agent-harness/harness.py
python3 examples/research-showcases/multi-agent-harness/test_harness.py
```

The default is `--dry-run`. It runs the planner and coder against memory,
verifies a locally signed webhook, and runs the candidate's real tests in a
temporary folder. It prints the patch from subtraction to addition. Its API,
cap and read-only mount are simulated. It creates no account, filesystem,
network request or charge.

## Before and after

| Step | Separate disposable folders | One Mountable filesystem per coding run |
| --- | --- | --- |
| Planner → coder | Export the plan and repository, then import them in the next worker | Write the plan and repository into the shared folder through the SDK |
| Coder → verifier | Export the modified tree and arrange a notification | Write the fix and a completion marker. A signed webhook triggers verification |
| Verifier | Arrange a read-only copy | Mint a `ro` ticket and mount the same files with `--ro` |
| Usage | Attribute each worker's storage and transfer to the run | Set a monthly spend cap and rate limit when creating the run's filesystem |

The example uses the Python SDK for the read-write stages and the CLI for
the read-only verifier. These are local stage processes. When you move them
into separate sandboxes, pass the verifier only its one-time ticket on stdin.
Keep the reusable API key on your orchestrator.

## Run against Mountable

Use a Linux machine with `/dev/fuse`, FUSE mount permissions, Python 3.10+
and the Mountable CLI 0.5.0 or later. Install the Python SDK:

```sh
python3 -m pip install mountable-sdk==0.1.0 mountable==0.5.0
```

Set `MOUNTABLE_API_KEY` through your secret store and `MOUNTABLE_ORG_ID` to
your organisation ID. Use an organisation-wide key created by an owner or
admin. The organisation needs a saved payment method and room for a 1 GiB,
10,000-entry filesystem.

Forward an existing public HTTPS endpoint on port 443 to
`127.0.0.1:8787/hooks/mountable` on this machine. The reverse proxy must pass
the raw body and `webhook-id`, `webhook-timestamp` and `webhook-signature`
headers unchanged. The receiver binds loopback only. `--port` changes its local port. Keep the proxy running for the duration of the command.

```sh
python3 examples/research-showcases/multi-agent-harness/harness.py --live \
  --webhook-url https://YOUR-HOST/hooks/mountable
```

Live mode creates a new filesystem with a **100-cent monthly cap**, in your
organisation's billing currency, and **60 mutations per minute**. Both are
set in the create request. A cap stops billable work with
`402 filesystem_spending_cap_reached`. A rate limit returns `429 rate_limited`.
The example stops on either response and never retries writes. Other
filesystems keep their own limits. Your organisation's cap applies on top.

The coder writes `READY.json` after its source and tests. The receiver
checks the signature against the raw body and rejects timestamps outside
five minutes. It checks the filesystem and webhook IDs. It ignores duplicate
or older cursors as integers. File creates, updates and deletes are accepted.
A rename arrives as a delete and a create. A resync also reconciles the
completion marker, covering a batched or missed marker event.

The receiver acknowledges before verification. The verifier mints a `ro`
ticket, starts `mountable mount --ro --ticket-stdin --json`, and waits for
`mounted`. It checks the source hash and prints the patch. It runs tests without writing
bytecode and checks that a workspace write is refused. The API key
stays out of the mount and test environments. Read-only permissions protect
the Mountable workspace. Use a separate sandbox to isolate arbitrary code
from the rest of a machine.

## Finish the run

The command unmounts the verifier, revokes its session and removes its webhook
on success or failure. It prints the new filesystem ID immediately after
creation. **Delete that filesystem in the Mountable console when finished.**
API keys cannot delete a filesystem. Stored files continue to count toward
usage until deletion. Disconnecting a worker does not stop storage billing.
The cap reserves this month's peak storage and downloads, so deletion does
not return this month's reserved cents. No pricing or free allowance changes
are part of this example.

If creation times out, inspect the console for the `harness-…` filesystem
before rerunning. If unmounting fails, inspect the printed mount point and
run `mountable unmount PATH`. After a killed process, use the filesystem's
session and webhook lists to revoke or remove anything still present. The
in-memory inbox lasts for one run. A long-running service should persist its
cursor and results together.

## Connect your agents

Replace `planner()` and `coder()` with your trusted orchestrator calls. Keep
one filesystem per run, write the completion marker last, and give the
verifier a read-only ticket. Serialize candidates or use separate run
filesystems so a later edit cannot race a verifier.

- [Python SDK and Files API](https://mountable.io/docs/sdks?utm_source=trending-research-and-repo-showcases&utm_medium=showcase_pr&utm_content=showcase-01-multi-agent-harness)
- [Limits per end user](https://mountable.io/docs/end-user-limits?utm_source=trending-research-and-repo-showcases&utm_medium=showcase_pr&utm_content=showcase-01-multi-agent-harness)
- [Signed file-change webhooks](https://mountable.io/docs/webhooks?utm_source=trending-research-and-repo-showcases&utm_medium=showcase_pr&utm_content=showcase-01-multi-agent-harness)
- [Sandbox mount setup](https://mountable.io/docs/sandbox?utm_source=trending-research-and-repo-showcases&utm_medium=showcase_pr&utm_content=showcase-01-multi-agent-harness)
