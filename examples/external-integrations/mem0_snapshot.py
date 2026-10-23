"""Archive one user's paginated Mem0 records as a new Mountable JSON file."""

from __future__ import annotations

import json
import os
from datetime import UTC, datetime
from uuid import uuid4

from mem0 import MemoryClient
from mountable_sdk import Filesystem, Mountable


def archive_user_memories(
    memory: MemoryClient, filesystem: Filesystem, user_id: str, path: str
) -> int:
    """Read Mem0 pages, then make one create-only write. Return the record count.

    This is a point-in-time export of returned records, not a database backup
    or a restore operation. Concurrent Mem0 edits are not a consistent snapshot.
    """
    if not user_id:
        raise ValueError("Choose the authenticated user's Mem0 identifier")
    records: list[dict] = []
    seen: set[str] = set()
    for page in range(1, 101):
        response = memory.get_all(
            filters={"user_id": user_id}, page=page, page_size=100
        )
        batch = response.get("results")
        if not isinstance(batch, list):
            raise TypeError("Expected Mem0's paginated results response")
        for record in batch:
            if not isinstance(record, dict) or not isinstance(record.get("id"), str):
                raise TypeError("Mem0 returned a record without a string ID")
            if record.get("user_id", user_id) != user_id:
                raise ValueError("Mem0 returned a record for a different user")
            if record["id"] in seen:
                raise ValueError("Mem0 pagination repeated a record; export aborted")
            seen.add(record["id"])
            records.append(record)
        if len(records) > 1000:
            raise ValueError("Example export is limited to 1000 records")
        if not response.get("next"):
            break
        if not batch:
            raise ValueError("Mem0 returned an empty page with another page pending")
    else:
        raise ValueError("Mem0 pagination exceeded the example's page limit")

    content = json.dumps(
        {
            "format": "mountable-mem0-records-v1",
            "exported_at": datetime.now(UTC).isoformat(),
            "user_id": user_id,
            "records": records,
        },
        ensure_ascii=False,
        indent=2,
    ).encode("utf-8")
    if len(content) > 1_048_576:
        raise ValueError("Example export is limited to 1 MiB")
    filesystem.write(path, content, if_none_match=True)
    return len(records)


if __name__ == "__main__":
    # Both identifiers come from your backend, not from model tool arguments.
    memory = MemoryClient()
    try:
        with Mountable() as client:
            filesystem = client.filesystem(os.environ["MOUNTABLE_FILESYSTEM_ID"])
            path = f"mem0-exports/{uuid4().hex}.json"
            print(f"Export destination: {path}", flush=True)
            count = archive_user_memories(
                memory, filesystem, os.environ["MEM0_USER_ID"], path
            )
            print(f"Archived {count} records to {path}")
    finally:
        memory.client.close()
