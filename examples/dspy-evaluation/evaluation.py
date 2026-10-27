"""Save a tiny model-free DSPy evaluation and verify it in a fresh process."""

import argparse
import hashlib
import json
import os
import re

import dspy
from mountable_sdk import Filesystem, Mountable, MountableError

VERSION = "3.4.1"
FORMAT = "mountable-dspy-evaluation-v1"
FILES = ("dataset.json", "results.json")
MAX_BYTES = 1_048_576  # Entire bundle, including its manifest.
CASES = (
    {"id": "one", "question": "2 + 2", "answer": "4"},
    {"id": "two", "question": "3 + 3", "answer": "6"},
    {"id": "three", "question": "4 + 4", "answer": "8"},
)
PREDICTIONS = {"2 + 2": "4", "3 + 3": "6", "4 + 4": "9"}


class FixturePredictor(dspy.Module):
    """A fixed lookup table. No LM is configured or called."""

    def forward(self, question: str) -> dspy.Prediction:
        return dspy.Prediction(answer=PREDICTIONS[question])


def exact_match(example, prediction, trace=None) -> bool:
    return example.answer == prediction.answer


def encode(value: object) -> bytes:
    return (
        json.dumps(value, sort_keys=True, indent=2, allow_nan=False) + "\n"
    ).encode()


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def prefix(run: str) -> str:
    if not re.fullmatch(r"[a-f0-9]{16}", run):
        raise ValueError("run must be 16 lowercase hex characters")
    return f"dspy-evaluations/{run}"


def build() -> dict[str, bytes]:
    examples = [dspy.Example(**case).with_inputs("question") for case in CASES]
    result = dspy.Evaluate(
        devset=examples,
        metric=exact_match,
        num_threads=1,
        display_progress=False,
        display_table=False,
        max_errors=1,
    )(FixturePredictor())
    records = [
        {"id": example.id, "prediction": prediction.answer, "match": bool(score)}
        for example, prediction, score in result.results
    ]
    return {
        "dataset.json": encode(list(CASES)),
        "results.json": encode({"cases": records, "score_percent": result.score}),
    }


def publish(fs: Filesystem, run: str) -> str:
    base = prefix(run)
    files = build()
    manifest = encode(
        {
            "format": FORMAT,
            "dspy": VERSION,
            "run": run,
            "files": {
                name: {"size": len(raw), "sha256": digest(raw)}
                for name, raw in files.items()
            },
        }
    )
    if sum(map(len, files.values())) + len(manifest) > MAX_BYTES:
        raise ValueError("Bundle exceeds 1 MiB")
    # Create-only: inspect failures, clean this run, and use a new id to retry.
    for name, raw in files.items():
        fs.write(f"{base}/{name}", raw, if_none_match=True)
    # The completion record is written only after both data uploads succeed.
    fs.write(f"{base}/manifest.json", manifest, if_none_match=True)
    return digest(manifest)


def verify(fs: Filesystem, run: str, manifest_sha256: str) -> float:
    base = prefix(run)
    if not re.fullmatch(r"[a-f0-9]{64}", manifest_sha256):
        raise ValueError("Provide the digest from the trusted writer")
    raw = fs.read(f"{base}/manifest.json").data
    if len(raw) > 4096 or digest(raw) != manifest_sha256:
        raise ValueError("Manifest checksum or size mismatch")
    manifest = json.loads(raw)
    if (
        not isinstance(manifest, dict)
        or set(manifest) != {"format", "dspy", "run", "files"}
        or manifest["format"] != FORMAT
        or manifest["dspy"] != VERSION
        or manifest["run"] != run
        or not isinstance(manifest["files"], dict)
        or set(manifest["files"]) != set(FILES)
    ):
        raise ValueError("Unexpected manifest schema, version or paths")
    total = len(raw)
    for entry in manifest["files"].values():
        if (
            not isinstance(entry, dict)
            or set(entry) != {"size", "sha256"}
            or type(entry["size"]) is not int
            or not 0 < entry["size"] <= MAX_BYTES
            or not isinstance(entry["sha256"], str)
            or not re.fullmatch(r"[a-f0-9]{64}", entry["sha256"])
        ):
            raise ValueError("Invalid artifact size or checksum")
        total += entry["size"]
    if total > MAX_BYTES:
        raise ValueError("Bundle exceeds 1 MiB")
    documents = {}
    for name in FILES:
        data = fs.read(f"{base}/{name}").data
        entry = manifest["files"][name]
        if len(data) != entry["size"] or digest(data) != entry["sha256"]:
            raise ValueError(f"Artifact checksum or size mismatch: {name}")
        documents[name] = json.loads(data)
    dataset, results = documents[FILES[0]], documents[FILES[1]]
    if dataset != list(CASES):
        raise ValueError("Unexpected synthetic dataset")
    if (
        not isinstance(results, dict)
        or set(results) != {"cases", "score_percent"}
        or not isinstance(results["cases"], list)
        or len(results["cases"]) != len(dataset)
        or type(results["score_percent"]) not in (int, float)
    ):
        raise ValueError("Unexpected results schema")
    matches = []
    for case, saved in zip(dataset, results["cases"], strict=True):
        if (
            not isinstance(saved, dict)
            or set(saved) != {"id", "prediction", "match"}
            or saved["id"] != case["id"]
            or saved["prediction"] != PREDICTIONS[case["question"]]
            or type(saved["match"]) is not bool
        ):
            raise ValueError("Unexpected per-case result")
        match = case["answer"] == saved["prediction"]
        if saved["match"] != match:
            raise ValueError("Per-case metric mismatch")
        matches.append(match)
    score = round(100 * sum(matches) / len(matches), 2)
    if results["score_percent"] != score:
        raise ValueError("Aggregate score mismatch")
    return score


def cleanup(fs: Filesystem, run: str) -> None:
    base = prefix(run)
    # Fixed paths only; cleanup also handles a partially uploaded bundle.
    for name in ("manifest.json", *FILES):
        try:
            file = fs.read(f"{base}/{name}")
            if not file.info.etag:
                raise ValueError("Cannot safely delete a file without its ETag")
            fs.delete(f"{base}/{name}", if_match=file.info.etag)
        except MountableError as error:
            if error.code != "not_found":
                raise
    for path in (base, "dspy-evaluations"):
        try:
            fs.delete(path)
        except MountableError as error:
            if error.code not in ("not_found", "directory_not_empty"):
                raise


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("write", "read", "cleanup", "ro-write"))
    parser.add_argument("--run", required=True)
    parser.add_argument("--manifest-sha256", default="")
    args = parser.parse_args()
    try:
        prefix(args.run)
        with Mountable() as client:
            mode = "ro" if args.command in ("read", "ro-write") else "rw"
            fs = client.filesystem(os.environ["MOUNTABLE_FILESYSTEM_ID"], mode)
            if args.command == "write":
                print("manifest_sha256=" + publish(fs, args.run))
            elif args.command == "read":
                print(
                    f"match cases=3 score_percent={verify(fs, args.run, args.manifest_sha256):.2f}"
                )
            elif args.command == "ro-write":
                fs.write(f"{prefix(args.run)}/dataset.json", b"[]", if_none_match=True)
                raise ValueError("Read-only write unexpectedly succeeded")
            else:
                cleanup(fs, args.run)
                print("deleted " + prefix(args.run))
    except MountableError as error:
        parser.exit(1, f"{error.code}: inspect this run before retrying a write.\n")
    except (ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"Bundle refused: {error}\n")


if __name__ == "__main__":
    main()
