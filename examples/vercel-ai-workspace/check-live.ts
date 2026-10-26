// Uses the AI SDK's scripted provider, never a paid model provider.
import assert from "node:assert/strict";
import { generateText } from "ai";
import { MockLanguageModelV3 } from "ai/test";
import { configuredTools } from "./lib/tools.ts";

const action = process.argv[2];
if (action !== "create" && action !== "read") throw new Error("Use create or read");
const text = process.env.NOTE_TEXT ?? "Parser review complete. Check the file next.";
const toolName = action === "create" ? "create_note" : "read_note";
const model = new MockLanguageModelV3({
  doGenerate: async () => ({
    content: [{ type: "tool-call", toolCallId: "note", toolName,
      input: JSON.stringify(action === "create" ? { text } : {}) }],
    finishReason: { unified: "tool-calls", raw: undefined },
    usage: { inputTokens: { total: 0, noCache: 0, cacheRead: 0, cacheWrite: 0 },
      outputTokens: { total: 0, text: 0, reasoning: 0 } },
    warnings: [],
  }),
});
const result = await generateText({ model, tools: configuredTools(), prompt: "Use the configured note tool", maxRetries: 0 });
assert.equal(result.toolResults.length, 1, "tool failed: inspect storage state before retrying");
const output = result.toolResults[0].output;
assert.equal(output, action === "create" ? "stored" : text);
console.log(action === "create" ? "stored" : output);
