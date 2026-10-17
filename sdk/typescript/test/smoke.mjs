// Runs the built package (dist/) once against a local server, so CI can
// check it on the oldest supported Node: node test/smoke.mjs
import { createServer } from "node:http";
import { Mountable } from "../dist/index.js";

const server = createServer((req, res) => {
  let body = "";
  req.on("data", (chunk) => (body += chunk));
  req.on("end", () => {
    if (req.url === "/api/v1/mount-sessions") {
      const expires_at = new Date(Date.now() + 3600_000).toISOString();
      res.writeHead(201, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ ...JSON.parse(body), id: "s1", token: "mtblst_smoke", expires_at }));
      return;
    }
    res.writeHead(200, { ETag: '"e1"', "Last-Modified": new Date().toUTCString() });
    res.end("abc");
  });
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const client = new Mountable({ apiKey: "mtbl_smoke", baseUrl: `http://127.0.0.1:${server.address().port}` });
const file = await client.filesystem("fs_smoke").read("a b.txt");
server.close();
if (new TextDecoder().decode(file.data) !== "abc" || file.info.etag !== '"e1"') {
  throw new Error(`unexpected read: ${JSON.stringify(file.info)}`);
}
console.log(`mountable-sdk works on Node ${process.version}`);
