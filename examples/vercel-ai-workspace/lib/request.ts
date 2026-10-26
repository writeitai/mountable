import { timingSafeEqual } from "node:crypto";
import { MountableError } from "mountable-sdk";
import { z } from "zod";
import { configuredTools, noteInput } from "./tools.ts";

const requestInput = z.discriminatedUnion("action", [
  z.object({ action: z.literal("create"), text: noteInput.shape.text }).strict(),
  z.object({ action: z.literal("read") }).strict(),
]);
const execution = { toolCallId: "operator", messages: [], context: {} };

export async function handleNote(request: Request, makeTools = configuredTools) {
  const secret = process.env.DEMO_ACCESS_TOKEN;
  if (!secret || secret.length < 32) {
    return Response.json({ error: "Configure a DEMO_ACCESS_TOKEN of at least 32 characters" }, { status: 503 });
  }
  const provided = Buffer.from(request.headers.get("authorization") ?? "");
  const expected = Buffer.from(`Bearer ${secret}`);
  if (provided.length !== expected.length || !timingSafeEqual(provided, expected)) {
    return Response.json({ error: "Unauthorized" }, { status: 401 });
  }
  try {
    // Bound chunked requests too, before parsing or opening a storage session.
    const chunks: Uint8Array[] = [];
    let size = 0;
    const reader = request.body?.getReader();
    if (!reader) return Response.json({ error: "JSON required" }, { status: 400 });
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > 32 * 1024) {
        await reader.cancel();
        return Response.json({ error: "Request exceeds 32 KiB" }, { status: 413 });
      }
      chunks.push(value);
    }
    const input = requestInput.parse(JSON.parse(Buffer.concat(chunks).toString("utf8")));
    const tools = makeTools();
    const result = input.action === "create"
      ? await tools.create_note.execute!({ text: input.text }, execution)
      : await tools.read_note.execute!({}, execution);
    return Response.json({ result }, { headers: { "Cache-Control": "no-store" } });
  } catch (error) {
    if (error instanceof z.ZodError || error instanceof SyntaxError) {
      return Response.json({ error: "Use create with 1–16 KiB text, or read, without extra fields" }, { status: 400 });
    }
    if (error instanceof MountableError) {
      // Do not return server credentials, provider messages or stack traces.
      const message = error.status === 402 ? "Storage billing limit reached. Stop this user's work."
        : error.status === 412 ? "Note already exists. Read it before choosing a new path."
        : error.status === 404 ? "Note not found." : "Storage request failed.";
      return Response.json({ error: message }, { status: error.status === 402 || error.status === 412 || error.status === 404 ? error.status : 502 });
    }
    return Response.json({ error: "Check the server configuration and note format" }, { status: 500 });
  }
}
