/** Offline checks: real LangGraph nodes, a fake filesystem, no network. */

import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { MountableError } from "mountable-sdk";

import { buildGraphs, notePath, runGraph, type NoteStore } from "../graph.ts";

class Notes implements NoteStore {
  id: string;
  mode: "ro" | "rw";
  files: Map<string, Uint8Array>;
  readData: Uint8Array | null;
  writes: Array<[string, string]>;
  reads: string[];

  constructor(options: { id?: string; mode?: "ro" | "rw"; files?: Record<string, Uint8Array>; readData?: Uint8Array } = {}) {
    this.id = options.id ?? "chosen-fs";
    this.mode = options.mode ?? "rw";
    this.files = new Map(Object.entries(options.files ?? {}));
    this.readData = options.readData ?? null;
    this.writes = [];
    this.reads = [];
  }

  async write(path: string, data: string, options: { ifNoneMatch: boolean }): Promise<void> {
    assert.equal(options.ifNoneMatch, true);
    this.writes.push([path, data]);
    if (this.files.has(path)) {
      throw new MountableError(412, "precondition_failed", "private detail");
    }
    this.files.set(path, Buffer.from(data, "utf8"));
  }

  async read(path: string): Promise<{ data: Uint8Array }> {
    this.reads.push(path);
    if (this.readData !== null) {
      return { data: this.readData };
    }
    const data = this.files.get(path);
    if (data === undefined) {
      throw new MountableError(404, "not_found", "private path detail");
    }
    return { data };
  }
}

const BOUND = "chosen-fs";

describe("langgraph.js note handoff", () => {
  it("reads the filesystem, not the checkpoint text", async () => {
    const store = new Notes({ readData: Buffer.from("from-the-filesystem") });
    const { handoff } = buildGraphs(store, BOUND);
    const { result } = await runGraph(handoff, { path: "notes/brief.md", text: "planned body" });
    assert.deepEqual(store.writes, [["notes/brief.md", "planned body"]]);
    assert.deepEqual(store.reads, ["notes/brief.md"]);
    assert.equal(result.text, "from-the-filesystem");
    assert.equal(result.error, "");
  });

  it("enters read_note with empty text", async () => {
    const store = new Notes();
    const { handoff } = buildGraphs(store, BOUND);
    const { threadId, result } = await runGraph(handoff, { path: "notes/brief.md", text: "planned body" });
    const readerSteps = [];
    for await (const step of handoff.getStateHistory({ configurable: { thread_id: threadId } })) {
      if (step.next.includes("read_note")) {
        readerSteps.push(step);
      }
    }
    assert.ok(readerSteps.length > 0);
    assert.ok(readerSteps.every((step) => step.values.text === ""));
    assert.equal(result.text, "planned body");
  });

  it("lets a fresh checkpointer read the note from an earlier run", async () => {
    const store = new Notes();
    const first = buildGraphs(store, BOUND);
    await runGraph(first.handoff, { path: "notes/brief.md", text: "still here" });
    const second = buildGraphs(store, BOUND);
    const { result } = await runGraph(second.resume, { path: "notes/brief.md" });
    assert.equal(result.text, "still here");
    assert.equal(result.error, "");
    assert.deepEqual(store.writes, [["notes/brief.md", "still here"]]);
    assert.deepEqual(store.reads, ["notes/brief.md", "notes/brief.md"]);
  });

  it("rejects paths that escape notes/", async () => {
    const store = new Notes();
    const { handoff, resume } = buildGraphs(store, BOUND);
    for (const path of ["../secret.md", "/notes/brief.md", "notes/../brief.md", "notes/a.txt", "brief.md"]) {
      for (const [graph, state] of [
        [handoff, { path, text: "x" }],
        [resume, { path }],
      ] as const) {
        const { result } = await runGraph(graph, state);
        assert.notEqual(result.error, "");
        assert.equal(result.text, "");
      }
    }
    assert.deepEqual(store.writes, []);
    assert.deepEqual(store.reads, []);
  });

  it("refuses an oversized note before a write", async () => {
    const store = new Notes();
    const { handoff } = buildGraphs(store, BOUND);
    const { result } = await runGraph(handoff, { path: "notes/brief.md", text: "x".repeat(16_384 + 1) });
    assert.equal(result.error, "note is larger than 16 KiB");
    assert.deepEqual(store.writes, []);
  });

  it("stops at a spending cap without a second write", async () => {
    const store = new Notes();
    store.write = async (path, data) => {
      store.writes.push([path, data]);
      throw new MountableError(402, "filesystem_spending_cap_reached", "private balance");
    };
    const { handoff } = buildGraphs(store, BOUND);
    const { result } = await runGraph(handoff, { path: "notes/brief.md", text: "hello" });
    assert.deepEqual(store.writes, [["notes/brief.md", "hello"]]);
    assert.deepEqual(store.reads, []);
    assert.match(result.error, /filesystem_spending_cap_reached/);
    assert.equal(result.error.includes("private balance"), false);
    assert.equal(result.text, "");
  });

  it("does not mention writing again when a read's outcome is unknown", async () => {
    const store = new Notes();
    store.read = async (path) => {
      store.reads.push(path);
      throw new MountableError(null, "network_error", "connection reset");
    };
    const { resume } = buildGraphs(store, BOUND);
    const { result } = await runGraph(resume, { path: "notes/brief.md" });
    assert.equal(result.error, "network_error: outcome unknown.");
    assert.equal(result.error.includes("writing"), false);
  });

  it("tells the caller to read before retrying an unknown write", async () => {
    const store = new Notes();
    store.write = async (path, data) => {
      store.writes.push([path, data]);
      throw new MountableError(null, "network_error", "connection reset");
    };
    const { handoff } = buildGraphs(store, BOUND);
    const { result } = await runGraph(handoff, { path: "notes/brief.md", text: "hello" });
    assert.match(result.error, /outcome unknown/);
    assert.deepEqual(store.reads, []);
  });

  it("does not replace an existing note", async () => {
    const store = new Notes({ files: { "notes/brief.md": Buffer.from("already") } });
    const { handoff } = buildGraphs(store, BOUND);
    const { result } = await runGraph(handoff, { path: "notes/brief.md", text: "new" });
    assert.match(result.error, /precondition_failed/);
    assert.equal(Buffer.from(store.files.get("notes/brief.md") ?? new Uint8Array()).toString(), "already");
    assert.deepEqual(store.reads, []);
  });

  it("reports a read error without the server message", async () => {
    const store = new Notes();
    const { resume } = buildGraphs(store, BOUND);
    const { result } = await runGraph(resume, { path: "notes/brief.md" });
    assert.equal(result.error, "not_found");
    assert.equal(result.text, "");
  });

  it("denies a filesystem id that is not the one the graph is bound to", async () => {
    const store = new Notes();
    const { handoff, resume } = buildGraphs(store, BOUND);
    const handoffResult = await runGraph(handoff, {
      path: "notes/brief.md",
      text: "nope",
      filesystemId: "other-fs",
    });
    const resumeResult = await runGraph(resume, { path: "notes/brief.md", filesystemId: "other-fs" });
    assert.match(handoffResult.result.error, /different filesystem/);
    assert.match(resumeResult.result.error, /different filesystem/);
    assert.deepEqual(store.writes, []);
    assert.deepEqual(store.reads, []);
  });

  it("denies a store opened on a different filesystem", async () => {
    const store = new Notes({ id: "other-fs" });
    const { handoff } = buildGraphs(store, BOUND);
    const { result } = await runGraph(handoff, { path: "notes/brief.md", text: "nope" });
    assert.match(result.error, /different filesystem/);
    assert.deepEqual(store.writes, []);
  });

  it("does not write through a read-only handle", async () => {
    const store = new Notes({ mode: "ro" });
    const { handoff } = buildGraphs(store, BOUND);
    const { result } = await runGraph(handoff, { path: "notes/brief.md", text: "hello" });
    assert.match(result.error, /cannot write/);
    assert.deepEqual(store.writes, []);
  });

  it("accepts a plain markdown name", () => {
    assert.equal(notePath("notes/brief.md"), "notes/brief.md");
  });
});
