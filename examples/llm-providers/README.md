# LLM provider starters

Minimal orchestrator tools for a persistent Mountable workspace inside an
ephemeral sandbox. They use the standard library only.

`MOUNTABLE_API_KEY` stays in the orchestrator environment. `prepare` runs
`mountable ticket create --json` there. The sandbox receives the one-time
ticket on stdin of `mountable mount --ticket-stdin --json /mnt/work`. The
ticket is single-use and must be exchanged within five minutes.

| Provider | Module | Registration |
| --- | --- | --- |
| OpenAI Responses API and Agents SDK | `openai_responses.py` | `TOOL`, or `function_tool(create_mount_session)` |
| Anthropic Messages API and Claude Code | `anthropic_tools.py` | `TOOL`; Claude Code uses MCP `create_mount_ticket` |
| Gemini function calling and ADK | `gemini_adk.py` | `tools=[create_mount_session]` |
| xAI Grok | `xai_grok.py` | `TOOL` with `base_url="https://api.x.ai/v1"` |

In the sandbox, run `mkdir -p /mnt/work` before starting `sandbox["argv"]`.
Start that argv as a child and leave it running. Write `sandbox["stdin"]` to
that child's stdin. Use `mount.env` from `prepare`, or `sandbox_env` when
you only have the tool result, so the API key is absent. Read stdout until
the `mounted` event before using the directory. Do not wait for the mount
process to exit; it stays up to renew the session certificate. Revoke from
the orchestrator with `mountable sessions revoke SESSION_ID`.

The website guide is <https://mountable.io/docs/integrations>.

```sh
python3 examples/llm-providers/test_starters.py
```
