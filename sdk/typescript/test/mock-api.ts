// A local HTTP server that plays the Mountable API for the contract tests.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

export const API_KEY = "mtbl_testkeysecret";
export const FS = "fs_one";
export const FILES = `/api/v1/filesystems/${FS}/files`;
export const LIST = `/api/v1/filesystems/${FS}/list`;
export const CHANGES = `/api/v1/filesystems/${FS}/changes`;
export const SESSIONS = "/api/v1/mount-sessions";
export const ME = "/api/v1/me";
export const ORG = "org_one";
export const FILESYSTEMS = `/api/v1/orgs/${ORG}/filesystems`;
export const WEBHOOKS = `${FILESYSTEMS}/${FS}/webhooks`;
export const WEBHOOK_SECRET = "whsec_c2VjcmV0K2tleQ+/=";

export interface Request {
  method: string;
  /** As sent, still percent-encoded, without the query. */
  path: string;
  query: Record<string, string>;
  /** Lower-case names. */
  headers: Record<string, string>;
  body: Buffer;
  json(): any;
}

export interface Reply {
  status: number;
  body?: Buffer | string | object;
  headers?: Record<string, string>;
  /** "drop": close the connection without an answer; "short": send the headers and only part of the body. */
  fault?: "drop" | "short";
  /** Milliseconds to wait before answering. */
  delay?: number;
}

export type Handler = (request: Request) => Reply;

export function reply(status: number, body?: Reply["body"], headers?: Record<string, string>): Reply {
  return { status, body, headers };
}

export function error(status: number, code: string, fields: Record<string, unknown> = {}): Reply {
  return { status, body: { detail: { code, ...fields } } };
}

export function fileInfo(path: string, overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    path,
    type: "file",
    size: 3,
    etag: '"e1"',
    modified_at: "2026-10-17T12:00:00.123456789Z",
    ...overrides,
  };
}

/** GET /api/v1/me for the test API key, which belongs to ORG. */
export function me(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "u1",
    email: "dev@example.test",
    name: "Dev",
    organisations: [{ id: ORG, name: "One" }],
    api_key: { id: "k1", org_id: ORG },
    ...overrides,
  };
}

export function filesystemView(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: FS,
    name: "notes",
    byte_quota: 2 ** 30,
    entry_quota: 10_000,
    spending_cap_cents: null,
    mutations_per_minute: null,
    created_at: "2026-10-01T09:30:00Z",
    ...overrides,
  };
}

export function webhookView(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "wh_one",
    filesystem_id: FS,
    url: "https://example.test/hook",
    path_prefix: "/",
    state: "active",
    disabled_reason: null,
    created_by: "api key test",
    created_at: "2026-10-01T09:30:00Z",
    last_success_at: null,
    last_failure_at: null,
    last_failure: null,
    failing_since: null,
    ...overrides,
  };
}

/**
 * Answers "METHOD /path" routes and records every request. Token sessions
 * are issued by default: mtblst_token1secret, then mtblst_token2secret, …,
 * each expiring after sessionLifetimeMs. GET /api/v1/me answers me() by
 * default.
 */
export class MockAPI {
  routes = new Map<string, Handler>();
  requests: Request[] = [];
  sessionLifetimeMs = 3600_000;
  tokens = 0;
  url = "";
  #server: Server;

  constructor() {
    this.#server = createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (chunk: Buffer) => chunks.push(chunk));
      req.on("end", () => {
        const url = new URL(req.url ?? "/", "http://localhost");
        const rawPath = (req.url ?? "/").split("?")[0];
        const body = Buffer.concat(chunks);
        const headers: Record<string, string> = {};
        for (const [name, value] of Object.entries(req.headers)) {
          headers[name] = Array.isArray(value) ? value.join(", ") : (value ?? "");
        }
        const request: Request = {
          method: req.method ?? "GET",
          path: rawPath,
          query: Object.fromEntries(url.searchParams),
          headers,
          body,
          json: () => JSON.parse(body.toString()),
        };
        this.requests.push(request);
        const answer = this.answer(request);
        if (answer.delay) {
          setTimeout(() => this.#respond(req, res, answer), answer.delay);
        } else {
          this.#respond(req, res, answer);
        }
      });
    });
  }

  #respond(req: IncomingMessage, res: ServerResponse, answer: Reply): void {
    if (answer.fault === "drop") {
      req.socket.destroy();
      return;
    }
    let data: Buffer;
    const extra: Record<string, string> = { ...answer.headers };
    if (Buffer.isBuffer(answer.body)) {
      data = answer.body;
    } else if (typeof answer.body === "string") {
      data = Buffer.from(answer.body);
    } else if (answer.body === undefined) {
      data = Buffer.alloc(0);
    } else {
      data = Buffer.from(JSON.stringify(answer.body));
      extra["Content-Type"] ??= "application/json";
    }
    extra["Content-Length"] ??= String(data.length);
    extra.Connection = "close";
    res.writeHead(answer.status, extra);
    if (answer.fault === "short") {
      // The headers and half the body go out; then the connection ends.
      res.flushHeaders();
      res.write(data.subarray(0, Math.floor(data.length / 2)), () => res.socket?.end());
      return;
    }
    res.end(data);
  }

  async start(): Promise<void> {
    await new Promise<void>((resolve) => this.#server.listen(0, "127.0.0.1", resolve));
    this.url = `http://127.0.0.1:${(this.#server.address() as AddressInfo).port}`;
  }

  async close(): Promise<void> {
    this.#server.closeAllConnections();
    await new Promise((resolve) => this.#server.close(resolve));
  }

  route(method: string, path: string, handler: Handler | Reply): void {
    this.routes.set(`${method} ${path}`, typeof handler === "function" ? handler : () => handler);
  }

  answer(request: Request): Reply {
    const handler = this.routes.get(`${request.method} ${request.path}`);
    if (handler) {
      return handler(request);
    }
    if (request.method === "POST" && request.path === SESSIONS) {
      return this.issueSession(request);
    }
    if (request.method === "GET" && request.path === ME) {
      return reply(200, me());
    }
    return error(404, "not_found");
  }

  issueSession(request: Request): Reply {
    const body = request.json();
    const number = ++this.tokens;
    const session: Record<string, unknown> = {
      id: `s${number}`,
      filesystem_id: body.filesystem_id,
      mode: body.mode,
      state: "active",
      created_by: "api key test",
      expires_at: new Date(Date.now() + this.sessionLifetimeMs).toISOString(),
      created_at: new Date().toISOString(),
      ticket: null,
      ticket_expires_at: null,
    };
    if (body.credential === "token") {
      session.token = `mtblst_token${number}secret`;
      session.credential = "token";
    } else {
      session.state = "requested";
      session.ticket = `mtbltk_ticket${number}secret`;
      session.ticket_expires_at = new Date().toISOString();
    }
    return { status: 201, body: session };
  }

  calls(): string[] {
    return this.requests.map((r) => `${r.method} ${r.path}`);
  }

  /** The requests other than session creation and GET /api/v1/me. */
  operations(): Request[] {
    return this.requests.filter((r) => r.path !== SESSIONS && r.path !== ME);
  }

  sessions(): Request[] {
    return this.requests.filter((r) => r.path === SESSIONS);
  }
}
