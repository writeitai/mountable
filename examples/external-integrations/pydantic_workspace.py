"""Pydantic AI tools bound to one backend-selected Mountable filesystem."""

from __future__ import annotations

import os
import re

from mountable_sdk import Filesystem, Mountable, MountableError
from pydantic_ai import Agent, RunContext
from pydantic_ai.models import Model


def note_path(name: str) -> str:
    """Accept a Markdown filename, not a path or a filesystem identifier."""
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,75}\.md", name):
        raise ValueError("Use a Markdown filename such as summary.md")
    return f"notes/{name}"


def build_agent(model: Model | str) -> Agent[Filesystem, str]:
    agent = Agent(
        model,
        deps_type=Filesystem,
        instructions=(
            "Use the note tools for persistent text. Never put passwords or API "
            "keys in notes. Creating a note never overwrites an existing note. "
            "On spending_cap_reached or filesystem_spending_cap_reached, stop. "
            "Report other tool errors instead of repeatedly retrying writes."
        ),
    )

    @agent.tool
    def read_note(ctx: RunContext[Filesystem], name: str) -> str:
        """Read a UTF-8 Markdown note by filename, for example summary.md."""
        try:
            data = ctx.deps.read(note_path(name)).data
            if len(data) > 16_384:
                return "Error: this tool reads notes up to 16 KiB."
            return data.decode("utf-8")
        except (ValueError, UnicodeError):
            return "Error: use a valid Markdown filename and UTF-8 text."
        except MountableError as error:
            return f"Mountable refused the read: {error.code}"

    @agent.tool
    def create_note(ctx: RunContext[Filesystem], name: str, text: str) -> str:
        """Create a new Markdown note. Existing notes are never overwritten."""
        try:
            path = note_path(name)
            if len(text.encode("utf-8")) > 16_384:
                return "Error: this tool creates notes up to 16 KiB."
            ctx.deps.write(path, text, if_none_match=True)
            return f"Created {name}"
        except ValueError:
            return "Error: use a Markdown filename such as summary.md."
        except MountableError as error:
            return f"Mountable refused the create: {error.code}"

    return agent


if __name__ == "__main__":
    # Your backend selects this filesystem from its authenticated user mapping.
    # Never let the model choose another user's filesystem or change the limits.
    with Mountable() as client:
        filesystem = client.filesystem(os.environ["MOUNTABLE_FILESYSTEM_ID"])
        result = build_agent(os.environ["PYDANTIC_AI_MODEL"]).run_sync(
            "Create summary.md with a short note about persistent agent files.",
            deps=filesystem,
        )
        print(result.output)
