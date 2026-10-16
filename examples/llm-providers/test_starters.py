"""The sandbox spec never carries MOUNTABLE_API_KEY."""

from __future__ import annotations

import json
import os
import unittest
from unittest.mock import patch

import anthropic_tools
import gemini_adk
import handoff
import openai_responses
import xai_grok

API_KEY = "mtbl_orchestrator_secret"
TICKET = "mtbltk_sandbox_ticket"
SESSION = "11111111-1111-1111-1111-111111111111"
PARENT = {
    "MOUNTABLE_API_KEY": API_KEY,
    "PATH": "/usr/bin",
    "HOME": "/tmp/orchestrator",
    "NOTE": f"prefix-{API_KEY}",
}


def _session(argv: list[str]) -> str:
    return json.dumps(
        {
            "id": SESSION,
            "ticket": TICKET,
            "state": "requested",
            "mode": "ro" if "--ro" in argv else "rw",
        }
    )


def fake_runner(argv: list[str], env: dict[str, str]) -> tuple[int, str, str]:
    if API_KEY in argv:
        raise AssertionError("API key reached ticket create argv")
    if env.get(handoff.API_KEY_ENV) != API_KEY:
        raise AssertionError("orchestrator env lost the API key")
    return 0, _session(argv), ""


class StarterTests(unittest.TestCase):
    def test_each_provider_hands_the_ticket_on_stdin_only(self) -> None:
        modules = (openai_responses, anthropic_tools, gemini_adk, xai_grok)
        with (
            patch.object(handoff, "_default_runner", fake_runner),
            patch.dict(os.environ, PARENT, clear=True),
        ):
            for module in modules:
                result = module.create_mount_session("fs_123", "job-1", False)
                encoded = json.dumps(result)
                self.assertNotIn(API_KEY, encoded)
                self.assertEqual(result["session_id"], SESSION)
                sandbox = result["sandbox"]
                self.assertEqual(set(sandbox), {"argv", "stdin"})
                self.assertEqual(sandbox["stdin"], f"{TICKET}\n")
                self.assertEqual(list(sandbox["argv"]), list(handoff.MOUNT_ARGV))
                self.assertNotIn(API_KEY, sandbox["argv"])

    def test_sandbox_env_drops_the_api_key(self) -> None:
        with (
            patch.object(handoff, "_default_runner", fake_runner),
            patch.dict(os.environ, PARENT, clear=True),
        ):
            mount = handoff.prepare("fs_123", "job-1", False)
        self.assertNotIn(handoff.API_KEY_ENV, mount.env)
        self.assertNotIn("NOTE", mount.env)
        self.assertEqual(mount.env["PATH"], "/usr/bin")
        self.assertEqual(mount.stdin, f"{TICKET}\n")

    def test_read_only_sets_ro(self) -> None:
        seen: dict[str, list[str]] = {}

        def runner(argv: list[str], env: dict[str, str]) -> tuple[int, str, str]:
            seen["argv"] = argv
            return fake_runner(argv, env)

        with patch.object(handoff, "_default_runner", runner), patch.dict(os.environ, PARENT, clear=True):
            openai_responses.create_mount_session("fs_123", "job-1", True)
        self.assertIn("--ro", seen["argv"])
        self.assertNotIn(API_KEY, seen["argv"])

    def test_missing_ticket_is_not_mounted(self) -> None:
        def runner(argv: list[str], env: dict[str, str]) -> tuple[int, str, str]:
            return 0, json.dumps({"id": SESSION, "ticket": None}), ""

        with (
            patch.object(handoff, "_default_runner", runner),
            patch.dict(os.environ, PARENT, clear=True),
        ):
            with self.assertRaises(handoff.TicketError) as caught:
                handoff.prepare("fs_123", "job-1")
        self.assertEqual(caught.exception.code, "ticket_unavailable")
        self.assertNotIn(TICKET, str(caught.exception))

    def test_cli_failure_does_not_echo_stdout(self) -> None:
        def runner(argv: list[str], env: dict[str, str]) -> tuple[int, str, str]:
            return 1, f"boom {TICKET}", ""

        with (
            patch.object(handoff, "_default_runner", runner),
            patch.dict(os.environ, PARENT, clear=True),
        ):
            with self.assertRaises(handoff.TicketError) as caught:
                handoff.prepare("fs_123", "job-1")
        self.assertEqual(caught.exception.code, "ticket_create_failed")
        self.assertNotIn(TICKET, str(caught.exception))
        self.assertNotIn(API_KEY, str(caught.exception))

    def test_cli_error_code_is_preserved(self) -> None:
        def runner(argv: list[str], env: dict[str, str]) -> tuple[int, str, str]:
            return (
                1,
                json.dumps(
                    {
                        "error": {
                            "code": "no_grant",
                            "message": "no grant",
                            "hint": "ask an owner or admin for a grant, or use `--ro`",
                            "idempotency_key": "job-1",
                            "session": {"ticket": TICKET},
                        }
                    }
                ),
                "",
            )

        with (
            patch.object(handoff, "_default_runner", runner),
            patch.dict(os.environ, PARENT, clear=True),
        ):
            with self.assertRaises(handoff.TicketError) as caught:
                handoff.prepare("fs_123", "job-1")
        self.assertEqual(caught.exception.code, "no_grant")
        text = str(caught.exception)
        self.assertIn("hint: ask an owner or admin for a grant, or use `--ro`", text)
        self.assertIn("idempotency_key: job-1", text)
        self.assertNotIn(TICKET, text)
        self.assertNotIn(API_KEY, text)
        self.assertNotIn("session", text)

    def test_rate_limit_includes_retry_after(self) -> None:
        def runner(argv: list[str], env: dict[str, str]) -> tuple[int, str, str]:
            return (
                1,
                json.dumps(
                    {
                        "error": {
                            "code": "rate_limited",
                            "message": "too many requests",
                            "hint": "wait `retry_after` seconds, then retry",
                            "retry_after": 7,
                            "idempotency_key": "job-1",
                        }
                    }
                ),
                "",
            )

        with (
            patch.object(handoff, "_default_runner", runner),
            patch.dict(os.environ, PARENT, clear=True),
        ):
            with self.assertRaises(handoff.TicketError) as caught:
                handoff.prepare("fs_123", "job-1")
        self.assertEqual(caught.exception.retry_after, 7)
        self.assertIn("retry_after: 7", str(caught.exception))
        self.assertIn("mkdir -p /mnt/work", handoff.DESCRIPTION)

    def test_api_key_is_rejected_as_an_argument(self) -> None:
        with patch.dict(os.environ, PARENT, clear=True):
            with self.assertRaises(handoff.TicketError) as caught:
                handoff.prepare(API_KEY, "job-1", runner=fake_runner)
        self.assertEqual(caught.exception.code, "invalid_argument")

    def test_tool_schemas(self) -> None:
        self.assertEqual(openai_responses.TOOL["name"], "create_mount_session")
        self.assertTrue(openai_responses.TOOL["strict"])
        self.assertEqual(openai_responses.TOOL["parameters"]["additionalProperties"], False)
        self.assertEqual(xai_grok.TOOL["name"], "create_mount_session")
        self.assertEqual(xai_grok.CLIENT_BASE_URL, "https://api.x.ai/v1")
        self.assertNotIn("additionalProperties", xai_grok.TOOL["parameters"])
        self.assertNotIn("strict", xai_grok.TOOL)
        for module in (openai_responses, anthropic_tools, gemini_adk, xai_grok):
            self.assertNotIn("until the mounted event", module.create_mount_session.__doc__)
            self.assertIn("wait for the mounted event", module.create_mount_session.__doc__)
        self.assertEqual(anthropic_tools.TOOL["name"], "create_mount_session")
        self.assertIn("input_schema", anthropic_tools.TOOL)
        self.assertEqual(anthropic_tools.MCP_TOOL_NAME, "create_mount_ticket")
        self.assertEqual(gemini_adk.FUNCTION_DECLARATION["name"], "create_mount_session")
        self.assertIn("MOUNTABLE_API_KEY", gemini_adk.create_mount_session.__doc__)
        self.assertEqual(handoff.revoke_argv(SESSION), ["mountable", "sessions", "revoke", SESSION])


if __name__ == "__main__":
    unittest.main()
