/**
 * LangGraph.js nodes that share one mountable.io filesystem across processes.
 *
 * The application chooses the filesystem and the note path. This example uses
 * LangGraph's in-memory checkpointer. The writer returns empty text, so the
 * next node receives no note body in its input and reads the file. A later
 * process, with a new in-memory checkpointer, reads the same path.
 *
 * The nodes are deterministic. They do not call a model, so a passing run
 * does not say anything about model behavior.
 */

import { randomUUID } from "node:crypto";
import { parseArgs } from "node:util";
import { pathToFileURL } from "node:url";

import { Annotation, END, MemorySaver, START, StateGraph } from "@langchain/langgraph";
import { MountableError } from "mountable-sdk";

export const MAX_NOTE_BYTES = 16_384;
const NOTE_PATH = /^notes\/[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\.md$/;

function latest(fallback: string) {
  return Annotation<string>({
    reducer: (_left, right) => right,
    default: () => fallback,
  });
}

const NoteAnnotation = Annotation.Root({
  path: latest(""),
  text: latest(""),
  error: latest(""),
  filesystemId: latest(""),
});

export type NoteState = typeof NoteAnnotation.State;

export interface NoteFile {
  data: Uint8Array;
}

export interface NoteStore {
  id: string;
  mode: "ro" | "rw";
  write(path: string, data: string, options: { ifNoneMatch: boolean }): Promise<unknown>;
  read(path: string): Promise<NoteFile>;
}

/** Accept one Markdown note under notes/, not a filesystem id or a path escape. */
export function notePath(path: string): string {
  if (typeof path !== "string" || NOTE_PATH.test(path) === false) {
    throw new Error("use a path like notes/brief.md");
  }
  return path;
}

export function noteText(text: string): string {
  if (Buffer.byteLength(text, "utf8") > MAX_NOTE_BYTES) {
    throw new Error("note is larger than 16 KiB");
  }
  return text;
}

function refused(error: MountableError, writing: boolean): string {
  if (error.status === null) {
    if (writing) {
      return "network_error: outcome unknown. Read the note before writing again.";
    }
    return "network_error: outcome unknown.";
  }
  if (error.status === 412) {
    return "precondition_failed: the note already exists. Read it instead.";
  }
  if (error.code === "filesystem_spending_cap_reached" || error.code === "spending_cap_reached") {
    return `${error.code}: this user's billable mountable.io work stops.`;
  }
  return error.code;
}

function foreignFilesystem(store: NoteStore, filesystemId: string, state: NoteState): string | null {
  if (store.id !== filesystemId) {
    return "filesystem_denied: this graph is bound to a different filesystem.";
  }
  if (state.filesystemId !== "" && state.filesystemId !== filesystemId) {
    return "filesystem_denied: this graph is bound to a different filesystem.";
  }
  return null;
}

export function buildGraphs(store: NoteStore, filesystemId: string) {
  if (filesystemId === "") {
    throw new Error("a filesystem id is required");
  }

  async function writeNote(state: NoteState): Promise<Partial<NoteState>> {
    const foreign = foreignFilesystem(store, filesystemId, state);
    if (foreign !== null) {
      return { text: "", error: foreign };
    }
    if (store.mode !== "rw") {
      return { text: "", error: "permission_denied: this handle cannot write." };
    }
    let path: string;
    let text: string;
    try {
      path = notePath(state.path);
      text = noteText(state.text);
    } catch (error) {
      const message = error instanceof Error ? error.message : "invalid note";
      return { text: "", error: message };
    }
    try {
      await store.write(path, text, { ifNoneMatch: true });
    } catch (error) {
      if (error instanceof MountableError) {
        return { text: "", error: refused(error, true) };
      }
      throw error;
    }
    // read_note receives empty text. Earlier checkpoint steps still hold the input.
    return { text: "", error: "" };
  }

  async function readNote(state: NoteState): Promise<Partial<NoteState>> {
    const foreign = foreignFilesystem(store, filesystemId, state);
    if (foreign !== null) {
      return { text: "", error: foreign };
    }
    let path: string;
    try {
      path = notePath(state.path);
    } catch (error) {
      const message = error instanceof Error ? error.message : "invalid note";
      return { text: "", error: message };
    }
    let data: Uint8Array;
    try {
      data = (await store.read(path)).data;
    } catch (error) {
      if (error instanceof MountableError) {
        return { text: "", error: refused(error, false) };
      }
      throw error;
    }
    if (data.byteLength > MAX_NOTE_BYTES) {
      return { text: "", error: "note is larger than 16 KiB" };
    }
    try {
      const text = new TextDecoder("utf-8", { fatal: true }).decode(data);
      return { text, error: "" };
    } catch {
      return { text: "", error: "note is not UTF-8 text" };
    }
  }

  function afterWrite(state: NoteState): "read_note" | typeof END {
    return state.error === "" ? "read_note" : END;
  }

  const handoff = new StateGraph(NoteAnnotation)
    .addNode("write_note", writeNote)
    .addNode("read_note", readNote)
    .addEdge(START, "write_note")
    .addConditionalEdges("write_note", afterWrite, ["read_note", END])
    .addEdge("read_note", END)
    .compile({ checkpointer: new MemorySaver() });

  const resume = new StateGraph(NoteAnnotation)
    .addNode("read_note", readNote)
    .addEdge(START, "read_note")
    .addEdge("read_note", END)
    .compile({ checkpointer: new MemorySaver() });

  return { handoff, resume };
}

export async function runGraph(
  graph: {
    invoke: (state: Partial<NoteState>, config: { configurable: { thread_id: string } }) => Promise<NoteState>;
    getStateHistory: (config: { configurable: { thread_id: string } }) => AsyncIterable<{
      next: readonly string[];
      values: Partial<NoteState>;
    }>;
  },
  state: Partial<NoteState>,
): Promise<{ threadId: string; result: NoteState }> {
  const threadId = randomUUID().replaceAll("-", "");
  const result = await graph.invoke(state, { configurable: { thread_id: threadId } });
  return { threadId, result };
}

async function main(): Promise<void> {
  const { positionals, values } = parseArgs({
    args: process.argv.slice(2),
    allowPositionals: true,
    options: {
      filesystem: { type: "string" },
      path: { type: "string" },
      text: { type: "string", default: "" },
    },
  });
  const command = positionals[0];
  if (command !== "handoff" && command !== "resume") {
    throw new Error("command must be handoff or resume");
  }
  const filesystem = values.filesystem ?? "";
  const path = values.path ?? "";
  if (filesystem === "" || path === "") {
    throw new Error("pass --filesystem and --path");
  }

  const { Mountable } = await import("mountable-sdk");
  const mode = command === "handoff" ? "rw" : "ro";
  const client = new Mountable();
  const store = client.filesystem(filesystem, mode);
  const graphs = buildGraphs(store, filesystem);
  const graph = command === "handoff" ? graphs.handoff : graphs.resume;
  const state: Partial<NoteState> = { path };
  if (command === "handoff") {
    state.text = values.text ?? "";
  }
  const { threadId, result } = await runGraph(graph, state);
  const error = result.error ?? "";
  console.log(`thread_id=${threadId}`);
  console.log(`path=${path}`);
  if (error !== "") {
    console.log(`error=${error}`);
    process.exitCode = 1;
    return;
  }
  console.log(result.text ?? "");
}

const entry = process.argv[1];
if (entry !== undefined && import.meta.url === pathToFileURL(entry).href) {
  main().catch((error: unknown) => {
    const message = error instanceof Error ? error.message : "the graph failed";
    console.error(message);
    process.exitCode = 1;
  });
}
