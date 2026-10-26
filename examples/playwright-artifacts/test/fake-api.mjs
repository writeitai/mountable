import { createServer } from "node:http";

export const API_KEY = "mtbl_testkeysecret";
export const FS = "fsone";

export function startFakeApi() {
  const files = new Map();
  const dirs = new Set();
  let failTrace = null;
  const requests = [];
  const server = createServer((req, res) => {
    const chunks = [];
    req.on("data", (chunk) => chunks.push(chunk));
    req.on("end", () => {
      const body = Buffer.concat(chunks);
      const path = (req.url ?? "/").split("?")[0];
      requests.push({ method: req.method, path, body });
      const send = (status, payload, headers = {}) => {
        const data = Buffer.isBuffer(payload)
          ? payload
          : Buffer.from(payload === undefined ? "" : JSON.stringify(payload));
        res.writeHead(status, {
          "Content-Length": String(data.length),
          Connection: "close",
          ...headers,
        });
        res.end(data);
      };
      if (req.method === "POST" && path === "/api/v1/mount-sessions") {
        const session = JSON.parse(body.toString());
        send(201, {
          id: "s1",
          filesystem_id: session.filesystem_id,
          mode: session.mode,
          credential: "token",
          token: "mtblst_token1secret",
          state: "active",
          expires_at: new Date(Date.now() + 3600_000).toISOString(),
          created_at: new Date().toISOString(),
        });
        return;
      }
      const prefix = `/api/v1/filesystems/${FS}/files/`;
      if (!path.startsWith(prefix)) {
        send(404, { detail: { code: "not_found" } }, { "Content-Type": "application/json" });
        return;
      }
      const filePath = decodeURIComponent(path.slice(prefix.length));
      if (req.method === "PUT") {
        if (filePath.endsWith("trace.zip") && failTrace) {
          send(failTrace.status, { detail: { code: failTrace.code } }, { "Content-Type": "application/json" });
          return;
        }
        if (files.has(filePath) && req.headers["if-none-match"] === "*") {
          send(
            412,
            { detail: { code: "precondition_failed", current_etag: '"e1"' } },
            { "Content-Type": "application/json" },
          );
          return;
        }
        files.set(filePath, Buffer.from(body));
        const parts = filePath.split("/");
        for (let i = 1; i < parts.length; i += 1) {
          dirs.add(parts.slice(0, i).join("/"));
        }
        send(
          201,
          {
            path: `/${filePath}`,
            type: "file",
            size: body.length,
            etag: '"e1"',
            modified_at: "2026-10-26T12:00:00.000Z",
          },
          { "Content-Type": "application/json" },
        );
        return;
      }
      if (req.method === "GET") {
        const stored = files.get(filePath);
        if (!stored) {
          send(404, { detail: { code: "not_found" } }, { "Content-Type": "application/json" });
          return;
        }
        send(200, stored, {
          ETag: '"e1"',
          "Last-Modified": "Mon, 26 Oct 2026 12:00:00 GMT",
          "Content-Type": "application/octet-stream",
        });
        return;
      }
      if (req.method === "DELETE") {
        if (files.has(filePath)) {
          files.delete(filePath);
          send(204);
          return;
        }
        if (dirs.has(filePath)) {
          const prefix = `${filePath}/`;
          const occupied = [...files.keys(), ...dirs].some((name) => name.startsWith(prefix));
          if (occupied) {
            send(409, { detail: { code: "directory_not_empty" } }, { "Content-Type": "application/json" });
            return;
          }
          dirs.delete(filePath);
          send(204);
          return;
        }
        send(404, { detail: { code: "not_found" } }, { "Content-Type": "application/json" });
        return;
      }
      send(404, { detail: { code: "not_found" } }, { "Content-Type": "application/json" });
    });
  });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address();
      resolve({
        url: `http://127.0.0.1:${port}`,
        files,
        dirs,
        requests,
        setFailTrace(failure) {
          failTrace = failure;
        },
        close() {
          return new Promise((done) => server.close(done));
        },
      });
    });
  });
}
