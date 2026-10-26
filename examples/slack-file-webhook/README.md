# Get a Slack message when an agent writes a file

A small receiver checks a signed `files.changed` webhook and posts the path
to a Slack incoming webhook.

mountable.io maintains this example. It is not a Slack app. The hosted
mountable.io service is paid. The example is open source. It creates no
account, filesystem or Slack app.

## Setup

Use Python 3.11 or newer. Set `MOUNTABLE_API_KEY` in your private
environment or secret manager. Use an existing mountable.io account and a
filesystem the key can write. Create an incoming webhook in Slack and set
`SLACK_WEBHOOK_URL` to its `https://hooks.slack.com/services/...` URL. That
URL is a secret. Do not commit it.

The receiver listens on `127.0.0.1:8080`. Put a public `https://` address on
port 443 in front of it, at `/hooks/mountable`. The guide is
[a Slack message when a file changes](https://mountable.io/docs/slack-file-webhook?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=example-slack-file-webhook).

File writes use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=documentation-and-llm-visibility&utm_medium=other&utm_content=example-slack-file-webhook).
Webhook deliveries are not billed.

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/slack-file-webhook
export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/..."
export MOUNTABLE_WEBHOOK_SECRET="$(mountable webhooks create "$FS_ID" https://example.com/hooks/mountable --path-prefix /notes)"
python3 receiver.py
```

The create command prints the signing secret once on stdout and the webhook
id on stderr. Keep the secret in the environment. Do not commit it.

Write a file after the receiver is up:

```sh
printf 'ready\n' | mountable files write "$FS_ID" notes/report.md --if-none-match
```

Slack gets `created /notes/report.md`.

Stop deliveries with `mountable webhooks delete "$FS_ID" "$WEBHOOK_ID"`.

## Offline check

```sh
python3 -m unittest discover -p 'test_*.py'
python3 receiver.py --check
```

These checks sign a delivery locally. They do not call mountable.io or Slack.

Slack's incoming webhook steps:
<https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks>
(read 2026-10-26).
