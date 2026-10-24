// Runs the built package (dist/) once against a local server, so CI can
// check it on the oldest supported Node: node test/smoke.mjs
import { createHmac } from "node:crypto";
import { createServer } from "node:http";
import { Mountable, verifyWebhook } from "../dist/index.js";

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
// verifyWebhook uses node:crypto's Web Crypto where there is no global one.
const secret = `whsec_${Buffer.alloc(32, 7).toString("base64")}`;
const body = JSON.stringify({
  type: "files.resync",
  timestamp: new Date().toISOString(),
  data: { webhook_id: "w1", filesystem_id: "fs_smoke", events: [], cursor: "7" },
});
const timestamp = String(Math.floor(Date.now() / 1000));
const signature = createHmac("sha256", Buffer.alloc(32, 7)).update(`msg_1.${timestamp}.${body}`).digest("base64");
const message = await verifyWebhook(
  secret,
  { "webhook-id": "msg_1", "webhook-timestamp": timestamp, "webhook-signature": `v1,${signature}` },
  body,
);
if (message.cursor !== "7" || message.type !== "files.resync") {
  throw new Error(`unexpected message: ${JSON.stringify(message)}`);
}
console.log(`mountable-sdk works on Node ${process.version}`);
