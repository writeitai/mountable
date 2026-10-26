"""Gradio demo: a note that survives a Space restart.

The note lives in a mountable.io filesystem, not in the Space container.
Restart the Space and the note is still there, because the app reads it
back from mountable.io on every load.

The API key stays server-side. The Space owner adds a filesystem-limited
key and the demo filesystem ID as Space secrets named MOUNTABLE_API_KEY
and MOUNTABLE_FS_ID. Visitors never see either value.
"""

from __future__ import annotations

import os

NOTE_PATH = "demo/agent-note.txt"
MAX_CHARS = 2000


def get_fs():
    from mountable_sdk import Mountable

    fs_id = os.environ.get("MOUNTABLE_FS_ID")
    if not fs_id:
        raise ValueError("Add the demo filesystem ID as the Space secret MOUNTABLE_FS_ID")
    client = Mountable(api_key=os.environ.get("MOUNTABLE_API_KEY"))
    return client.filesystem(fs_id, mode="rw")


def save_note(text: str, fs=None) -> str:
    text = (text or "").strip()
    if not text:
        return "Write a note first."
    if len(text) > MAX_CHARS:
        return f"Keep the note under {MAX_CHARS} characters."
    fs = fs or get_fs()
    fs.write(NOTE_PATH, text + "\n")
    return "Saved to mountable.io. Restart the Space: the note stays."


def read_note(fs=None) -> str:
    fs = fs or get_fs()
    try:
        return fs.read(NOTE_PATH).data.decode()
    except Exception as e:
        if getattr(e, "code", None) == "not_found":
            return "No note yet. Save one, then restart the Space."
        raise


def build_app():
    import gradio as gr

    with gr.Blocks(title="mountable.io: a filesystem that survives restarts") as demo:
        gr.Markdown(
            "# A filesystem that survives restarts\n\n"
            "[mountable.io](https://mountable.io) is a persistent filesystem "
            "for AI agents. This Space writes a note to mountable.io, not to "
            "its own disk. Restart the Space and the note is still here."
        )
        box = gr.Textbox(label="Agent note", lines=4, max_lines=8)
        status = gr.Textbox(label="Status", interactive=False)
        gr.Button("Save note").click(save_note, inputs=box, outputs=status)
        gr.Button("Read note back").click(lambda: read_note(), outputs=box)
        demo.load(lambda: read_note(), outputs=box)
    return demo


if __name__ == "__main__":
    build_app().launch()
