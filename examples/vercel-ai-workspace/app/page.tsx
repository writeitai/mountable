"use client";
import { useState } from "react";

export default function Page() {
  const [token, setToken] = useState("");
  const [text, setText] = useState("Parser review complete. Check the file next.");
  const [result, setResult] = useState("");
  const [busy, setBusy] = useState(false);
  async function run(action: "create" | "read") {
    setBusy(true);
    try {
      const response = await fetch("/api/note", {
        method: "POST",
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
        body: JSON.stringify(action === "create" ? { action, text } : { action }),
        cache: "no-store",
      });
      const body = await response.json();
      setResult(body.result ?? body.error);
    } catch { setResult("Request failed. Read the note before trying another write."); }
    finally { setBusy(false); }
  }
  return <main>
    <p>mountable.io · community example</p>
    <h1>AI SDK tools that keep a note</h1>
    <p>Create a note, restart this Next.js app, then read the same file. The server runs AI SDK tools backed by mountable-sdk. These buttons call the tools directly and make no model request.</p>
    <p>The hosted filesystem is paid. Use your own account and existing filesystem. This example is maintained by mountable.io, independently of Vercel.</p>
    <label>Operator access token (not your mountable.io API key)<br />
      <input type="password" value={token} onChange={e => setToken(e.target.value)} autoComplete="off" style={{ width: "100%" }} />
    </label>
    <p><label>Note, up to 16 KiB<br />
      <textarea value={text} onChange={e => setText(e.target.value)} rows={5} style={{ width: "100%" }} />
    </label></p>
    <button disabled={busy || !token} onClick={() => run("create")}>Create note once</button>{" "}
    <button disabled={busy || !token} onClick={() => run("read")}>Read stored note</button>
    <pre role="status" style={{ whiteSpace: "pre-wrap" }}>{busy ? "Working…" : result}</pre>
    <p>The backend selects one filesystem and note path. Existing notes are never overwritten. After a timeout, read before writing again.</p>
    <a href="https://mountable.io/docs/vercel-ai-sdk">Setup and tool-calling guide</a>
  </main>;
}
