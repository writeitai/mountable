// The Mountable files API, mount tickets and webhooks over HTTPS. The
// behaviour is the contract in sdk/contract.md, shared with the Python SDK.

import type { webcrypto } from "node:crypto";

const DEFAULT_BASE_URL = "https://mountable.io";
// Milliseconds a request may take; a change-feed poll may take its wait longer.
const TIMEOUT_MS = 60_000;
// A token session lasts an hour and is replaced this close to its expiry.
const SESSION_SECONDS = 3600;
const REFRESH_BEFORE_MS = 60_000;
// Seconds a webhook's timestamp may be from now.
const WEBHOOK_TOLERANCE = 300;

export type Mode = "ro" | "rw";
export type FileType = "file" | "directory" | "symlink";
export type ChangeType = "created" | "updated" | "deleted";
export type WebhookState = "active" | "disabled";

export interface FileInfo {
  path: string;
  type: FileType;
  size: number;
  etag: string | null;
  modifiedAt: Date;
}

export interface File {
  data: Uint8Array;
  info: FileInfo;
}

export interface ListPage {
  entries: FileInfo[];
  next: string | null;
}

export interface Change {
  type: ChangeType;
  path: string;
  file: FileInfo | null;
}

export interface ChangeBatch {
  events: Change[];
  cursor: string;
}

/** A mount session with its one-time ticket. The ticket is read from `.ticket` and is not shown when the object is printed. */
export class MountTicket {
  readonly id: string;
  readonly filesystemId: string;
  readonly mode: Mode;
  readonly state: string;
  readonly ticketExpiresAt: Date | null;
  readonly expiresAt: Date;
  readonly #ticket: string | null;

  /** @internal */
  constructor(session: any) {
    this.id = session.id;
    this.filesystemId = session.filesystem_id;
    this.mode = session.mode;
    this.state = session.state;
    this.#ticket = session.ticket ?? null;
    this.ticketExpiresAt = session.ticket_expires_at ? new Date(session.ticket_expires_at) : null;
    this.expiresAt = new Date(session.expires_at);
  }

  /** The one-time ticket for `mountable mount --ticket-stdin`; null on an idempotent replay. */
  get ticket(): string | null {
    return this.#ticket;
  }
}

/**
 * A filesystem's webhook. Its `secret` is set only in the result of
 * `createWebhook` and `rotateWebhookSecret`, the only time the API shows it,
 * and is not shown when the object is printed or serialised.
 */
export class Webhook {
  readonly id: string;
  readonly filesystemId: string;
  readonly url: string;
  readonly pathPrefix: string;
  readonly state: WebhookState;
  /** "failing" or "access_revoked". */
  readonly disabledReason: string | null;
  readonly createdBy: string;
  readonly createdAt: Date;
  readonly lastSuccessAt: Date | null;
  readonly lastFailureAt: Date | null;
  /** timeout, connect_failed, tls_failed, blocked_address, redirect or http_<status>. */
  readonly lastFailure: string | null;
  readonly failingSince: Date | null;
  readonly #secret: string | null;

  /** @internal */
  constructor(raw: any) {
    this.id = raw.id;
    this.filesystemId = raw.filesystem_id;
    this.url = raw.url;
    this.pathPrefix = raw.path_prefix;
    this.state = raw.state;
    this.disabledReason = raw.disabled_reason ?? null;
    this.createdBy = raw.created_by;
    this.createdAt = new Date(raw.created_at);
    this.lastSuccessAt = optionalDate(raw.last_success_at);
    this.lastFailureAt = optionalDate(raw.last_failure_at);
    this.lastFailure = raw.last_failure ?? null;
    this.failingSince = optionalDate(raw.failing_since);
    this.#secret = raw.secret ?? null;
  }

  /** The signing secret (whsec_…); null except right after create or rotate. */
  get secret(): string | null {
    return this.#secret;
  }
}

/** A verified webhook delivery. */
export interface WebhookMessage {
  /** The webhook-id header: the same on every retry of a message. */
  id: string;
  /** files.changed, or files.resync (no events: list what you need again). */
  type: string;
  timestamp: Date;
  webhookId: string;
  filesystemId: string;
  events: Change[];
  /**
   * The ordering key: apply a message only if its cursor, compared as an
   * integer, is greater than the last one applied.
   */
  cursor: string;
}

export interface CreateWebhookOptions {
  /** Deliver only changes in this directory; default "/". */
  pathPrefix?: string;
}

export interface MountableOptions {
  /** Defaults to MOUNTABLE_API_KEY. */
  apiKey?: string;
  /** Defaults to MOUNTABLE_API_URL, then https://mountable.io. */
  baseUrl?: string;
}

export interface CreateMountTicketOptions {
  mode?: Mode;
  maxDurationSeconds?: number;
  /** Reuse it to retry after an unknown outcome; random when omitted. */
  idempotencyKey?: string;
}

export interface WriteOptions {
  /** Write only if the file has this ETag; null or omitted: unconditional. */
  ifMatch?: string | null;
  ifNoneMatch?: boolean;
}

export interface DeleteOptions {
  ifMatch?: string | null;
}

export interface ListOptions {
  after?: string;
  limit?: number;
}

export interface ChangesOptions {
  cursor?: string;
  /** Seconds to wait for a change, 0–25; default 25. */
  wait?: number;
}

/**
 * One filesystem, used through a token session of its mode. The first
 * operation creates the session; every later one reuses it. A 401 or a
 * session within 60 seconds of its expiry makes the handle create a new one,
 * and a refused call is sent once more.
 */
export interface Filesystem {
  readonly id: string;
  readonly mode: Mode;
  /** Replaces or creates the whole file. Never retried by the SDK. */
  write(path: string, data: Uint8Array | string, options?: WriteOptions): Promise<FileInfo>;
  read(path: string): Promise<File>;
  /** Deletes a file, symlink or empty directory. Never retried. */
  delete(path: string, options?: DeleteOptions): Promise<void>;
  list(path?: string, options?: ListOptions): Promise<ListPage>;
  /**
   * Changes after `cursor`, waiting up to `wait` seconds for one. Without a
   * cursor, returns no events and the current cursor at once. To stay in
   * sync: take a cursor first, then list, then follow changes from it.
   * `cursor_expired` (410) means: do that again.
   */
  changes(options?: ChangesOptions): Promise<ChangeBatch>;
}

// Every credential the API issues starts with "mtbl": API keys (mtbl_),
// session tokens (mtblst_), tickets (mtbltk_) and login tokens. Webhook
// signing secrets are whsec_ and standard base64.
const SECRET = /(mtbl[a-z]*)_[A-Za-z0-9_.-]+/g;
const WEBHOOK_SECRET = /whsec_[A-Za-z0-9+/=]+/g;

function redact(text: string): string {
  return text.replace(SECRET, "$1_[redacted]").replace(WEBHOOK_SECRET, "whsec_[redacted]");
}

/**
 * A request failed: a non-2xx answer or a network failure. `status` is the
 * HTTP status, null when no answer arrived; `code` is the API's stable error
 * code, or `network_error`.
 */
export class MountableError extends Error {
  readonly status: number | null;
  readonly code: string;
  readonly currentEtag: string | null;
  readonly retryAfter: number | null;

  constructor(
    status: number | null,
    code: string,
    message: string,
    extra: { currentEtag?: string | null; retryAfter?: number | null } = {},
  ) {
    super(redact(message));
    this.name = "MountableError";
    this.status = status;
    this.code = redact(code);
    this.currentEtag = extra.currentEtag ?? null;
    this.retryAfter = extra.retryAfter ?? null;
  }
}

interface Session {
  token: string;
  expiresAt: Date;
}

interface SendOptions {
  body?: Uint8Array | string;
  headers?: Record<string, string>;
  query?: Record<string, string | number>;
  timeoutMs?: number;
}

/** An HTTP answer with its whole body. */
interface Answer {
  status: number;
  headers: Headers;
  data: Uint8Array;
}

type Send = (method: string, path: string, bearer: string, options?: SendOptions) => Promise<Answer>;

/** A client authenticated with an API key. */
export class Mountable {
  readonly baseUrl: string;
  // Private fields stay out of console.log and util.inspect.
  readonly #apiKey: string;
  // The API key's organisation, which webhook paths name.
  #orgId: Promise<string> | null = null;

  constructor(options: MountableOptions = {}) {
    const apiKey = options.apiKey || env("MOUNTABLE_API_KEY");
    if (!apiKey) {
      throw new MountableError(null, "unauthenticated", "pass apiKey or set MOUNTABLE_API_KEY");
    }
    this.#apiKey = apiKey;
    this.baseUrl = (options.baseUrl || env("MOUNTABLE_API_URL") || DEFAULT_BASE_URL).replace(/\/+$/, "");
  }

  /** A handle on one filesystem. It sends nothing until it is used. */
  filesystem(id: string, mode: Mode = "rw"): Filesystem {
    const send: Send = (method, path, bearer, options) => this.#send(method, path, bearer, options);
    return new FilesystemHandle(id, mode, send, () => this.#createTokenSession(id, mode));
  }

  /**
   * A one-time ticket for `mountable mount --ticket-stdin`. Pass your own
   * idempotencyKey and reuse it to retry after an unknown outcome. A replay
   * returns the existing session with `ticket` null.
   */
  async createMountTicket(fsId: string, options: CreateMountTicketOptions = {}): Promise<MountTicket> {
    const body: Record<string, unknown> = {
      filesystem_id: fsId,
      mode: options.mode ?? "rw",
      idempotency_key: options.idempotencyKey || (await randomKey()),
    };
    if (options.maxDurationSeconds !== undefined) {
      body.max_duration_seconds = options.maxDurationSeconds;
    }
    return new MountTicket(await this.#postSession(body));
  }

  /**
   * Calls `url` with the filesystem's changes under `pathPrefix`. The
   * result's `secret` signs the deliveries and is shown only now: store it
   * where your endpoint reads it. The first delivery, a files.resync, may
   * arrive before this resolves. Never retried: after an unknown outcome,
   * look for the URL in `listWebhooks` first.
   */
  async createWebhook(fsId: string, url: string, options: CreateWebhookOptions = {}): Promise<Webhook> {
    const path = await this.#webhooks(fsId);
    const body = { url, path_prefix: options.pathPrefix ?? "/" };
    return new Webhook(json(await this.#call("POST", path, false, body)));
  }

  async listWebhooks(fsId: string): Promise<Webhook[]> {
    const response = await this.#call("GET", await this.#webhooks(fsId), true);
    return json(response).map((raw: any) => new Webhook(raw));
  }

  async deleteWebhook(fsId: string, webhookId: string): Promise<void> {
    await this.#call("DELETE", await this.#webhooks(fsId, webhookId), false);
  }

  /**
   * A new secret, shown only now. For 24 hours deliveries are signed with
   * the previous one too; a second rotation in that time drops the oldest.
   */
  async rotateWebhookSecret(fsId: string, webhookId: string): Promise<Webhook> {
    const path = `${await this.#webhooks(fsId, webhookId)}:rotate-secret`;
    return new Webhook(json(await this.#call("POST", path, false)));
  }

  /** Resumes a webhook disabled as failing; one disabled as access_revoked rejects with access_revoked. */
  async enableWebhook(fsId: string, webhookId: string): Promise<Webhook> {
    const path = `${await this.#webhooks(fsId, webhookId)}:enable`;
    return new Webhook(json(await this.#call("POST", path, false)));
  }

  /** The path of a filesystem's webhooks, or of one. The first call looks up the API key's organisation. */
  async #webhooks(fsId: string, webhookId?: string): Promise<string> {
    // Concurrent first calls share one lookup; a failed one is not kept.
    this.#orgId ??= this.#lookUpOrg().catch((error) => {
      this.#orgId = null;
      throw error;
    });
    const path = `/api/v1/orgs/${encodeSegment(await this.#orgId)}/filesystems/${encodeSegment(fsId)}/webhooks`;
    return webhookId === undefined ? path : `${path}/${encodeSegment(webhookId)}`;
  }

  async #lookUpOrg(): Promise<string> {
    const response = await this.#call("GET", "/api/v1/me", true);
    const org = json(response).api_key?.org_id;
    if (typeof org !== "string" || !org) {
      throw new MountableError(response.status, "api_error", "the API did not report the API key's organisation");
    }
    return org;
  }

  /** Sends one request with the API key; with `retry`, once more after a network failure. */
  async #call(method: string, path: string, retry: boolean, body?: unknown): Promise<Answer> {
    const options: SendOptions =
      body === undefined ? {} : { body: JSON.stringify(body), headers: { "Content-Type": "application/json" } };
    let response: Answer;
    try {
      response = await this.#send(method, path, this.#apiKey, options);
    } catch (error) {
      if (!retry || !(error instanceof MountableError) || error.code !== "network_error") {
        throw error;
      }
      response = await this.#send(method, path, this.#apiKey, options);
    }
    if (!isSuccess(response)) {
      throw apiError(response);
    }
    return response;
  }

  async #postSession(body: Record<string, unknown>): Promise<any> {
    return json(await this.#call("POST", "/api/v1/mount-sessions", false, body));
  }

  async #createTokenSession(fsId: string, mode: Mode): Promise<Session> {
    const session = await this.#postSession({
      filesystem_id: fsId,
      mode,
      max_duration_seconds: SESSION_SECONDS,
      idempotency_key: await randomKey(),
      credential: "token",
    });
    if (typeof session.token !== "string") {
      throw new MountableError(201, "api_error", "the API returned no session token");
    }
    return { token: session.token, expiresAt: new Date(session.expires_at) };
  }

  async #send(method: string, path: string, bearer: string, options: SendOptions = {}): Promise<Answer> {
    const url = new URL(this.baseUrl + path);
    for (const [name, value] of Object.entries(options.query ?? {})) {
      url.searchParams.set(name, String(value));
    }
    const timeoutMs = options.timeoutMs ?? TIMEOUT_MS;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    // The timeout restarts whenever part of the answer arrives, as httpx's
    // read timeout does in the Python SDK.
    const arm = () => {
      clearTimeout(timer);
      timer = setTimeout(() => controller.abort(new Error(`no answer for ${timeoutMs / 1000} s`)), timeoutMs);
    };
    arm();
    try {
      const response = await fetch(url, {
        method,
        headers: { Authorization: `Bearer ${bearer}`, ...options.headers },
        body: options.body,
        // Only a 2xx answer is a success; a redirect is not followed.
        redirect: "manual",
        signal: controller.signal,
      });
      // Read the whole body here, so a body cut short is a network failure.
      const chunks: Uint8Array[] = [];
      const reader = response.body?.getReader();
      for (;;) {
        arm();
        const chunk = await reader?.read();
        if (!chunk || chunk.done) {
          break;
        }
        chunks.push(chunk.value);
      }
      return { status: response.status, headers: response.headers, data: concat(chunks) };
    } catch (error) {
      throw new MountableError(null, "network_error", `the API could not be reached: ${describe(error)}`);
    } finally {
      clearTimeout(timer);
    }
  }
}

class FilesystemHandle implements Filesystem {
  readonly id: string;
  readonly mode: Mode;
  readonly #send: Send;
  readonly #createSession: () => Promise<Session>;
  #session: Session | null = null;
  #creating: Promise<Session> | null = null;

  constructor(id: string, mode: Mode, send: Send, createSession: () => Promise<Session>) {
    this.id = id;
    this.mode = mode;
    this.#send = send;
    this.#createSession = createSession;
  }

  async write(path: string, data: Uint8Array | string, options: WriteOptions = {}): Promise<FileInfo> {
    const headers: Record<string, string> = { "Content-Type": "application/octet-stream" };
    if (options.ifMatch != null) {
      headers["If-Match"] = quoteEtag(options.ifMatch);
    }
    if (options.ifNoneMatch) {
      headers["If-None-Match"] = "*";
    }
    const body = typeof data === "string" ? new TextEncoder().encode(data) : data;
    const response = await this.#request("PUT", this.#url("files", path), false, { body, headers });
    return fileInfo(json(response));
  }

  async read(path: string): Promise<File> {
    const response = await this.#request("GET", this.#url("files", path), true);
    const data = response.data;
    const length = response.headers.get("Content-Length");
    const modified = response.headers.get("Last-Modified");
    if (modified === null) {
      throw new MountableError(response.status, "api_error", "the API sent no Last-Modified");
    }
    return {
      data,
      info: {
        path: `/${trimSlashes(path)}`,
        type: "file",
        size: length !== null ? Number(length) : data.length,
        etag: response.headers.get("ETag"),
        modifiedAt: new Date(modified),
      },
    };
  }

  async delete(path: string, options: DeleteOptions = {}): Promise<void> {
    const headers: Record<string, string> = {};
    if (options.ifMatch != null) {
      headers["If-Match"] = quoteEtag(options.ifMatch);
    }
    await this.#request("DELETE", this.#url("files", path), false, { headers });
  }

  async list(path = "", options: ListOptions = {}): Promise<ListPage> {
    const query: Record<string, string | number> = {};
    if (options.after !== undefined) {
      query.after = options.after;
    }
    if (options.limit !== undefined) {
      query.limit = options.limit;
    }
    const response = await this.#request("GET", this.#url("list", path), true, { query });
    const body = json(response);
    return { entries: body.entries.map(fileInfo), next: body.next ?? null };
  }

  async changes(options: ChangesOptions = {}): Promise<ChangeBatch> {
    const wait = options.wait ?? 25;
    const query: Record<string, string | number> = {};
    if (options.cursor !== undefined) {
      query.cursor = options.cursor;
      query.wait = wait;
    }
    const response = await this.#request("GET", `/api/v1/filesystems/${encodeSegment(this.id)}/changes`, true, {
      query,
      timeoutMs: TIMEOUT_MS + wait * 1000,
    });
    const body = json(response);
    return { events: body.events.map(change), cursor: body.cursor };
  }

  #url(kind: string, path: string): string {
    const trimmed = trimSlashes(path);
    const segments = trimmed ? trimmed.split("/") : [];
    if (segments.some((segment) => segment === "." || segment === "..")) {
      // URL normalisation would resolve them before the API sees them.
      throw new MountableError(null, "bad_path", `invalid path: ${JSON.stringify(path)}`);
    }
    return `/api/v1/filesystems/${encodeSegment(this.id)}/${kind}/${segments.map(encodeSegment).join("/")}`;
  }

  async #token(): Promise<Session> {
    const session = this.#session;
    if (session && session.expiresAt.getTime() - Date.now() > REFRESH_BEFORE_MS) {
      return session;
    }
    // Concurrent calls share one new session.
    this.#creating ??= this.#createSession()
      .then((created) => {
        this.#session = created;
        return created;
      })
      .finally(() => {
        this.#creating = null;
      });
    return this.#creating;
  }

  /**
   * Sends one call. A 401 creates a new session and sends the call once more,
   * once per call; a read is also sent once more after a network error.
   */
  async #request(method: string, path: string, retry: boolean, options?: SendOptions): Promise<Answer> {
    let refresh = true;
    let networkRetry = retry;
    for (;;) {
      let session: Session;
      let response: Answer;
      try {
        session = await this.#token();
        response = await this.#send(method, path, session.token, options);
      } catch (error) {
        if (!networkRetry || !(error instanceof MountableError) || error.code !== "network_error") {
          throw error;
        }
        networkRetry = false;
        continue;
      }
      if (response.status === 401 && refresh) {
        refresh = false;
        if (this.#session === session) {
          this.#session = null;
        }
        continue;
      }
      if (!isSuccess(response)) {
        throw apiError(response);
      }
      return response;
    }
  }
}

function isSuccess(response: Answer): boolean {
  return response.status >= 200 && response.status < 300;
}

/** Web Crypto: global in Bun, Deno and Node 19+; Node 18 has no global in ES modules. */
async function webCrypto(): Promise<webcrypto.Crypto> {
  return (globalThis as { crypto?: webcrypto.Crypto }).crypto ?? (await import("node:crypto")).webcrypto;
}

async function randomKey(): Promise<string> {
  const bytes = new Uint8Array(16);
  (await webCrypto()).getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

/** Headers as fetch-style servers (Headers) or Node's http module (an object) give them. */
export type WebhookHeaders = Headers | Record<string, string | string[] | undefined>;

/**
 * Checks a delivery's signature and resolves to its message. `headers` are
 * the request's (names in any case) and `body` its exact bytes or text, not
 * JSON parsed and serialised again. Rejects with `MountableError` code
 * `invalid_webhook` unless a v1 signature in webhook-signature is by
 * `secret` (Standard Webhooks) and webhook-timestamp is within five minutes
 * of now. A message can arrive more than once; see `WebhookMessage.cursor`.
 */
export async function verifyWebhook(
  secret: string,
  headers: WebhookHeaders,
  body: Uint8Array | string,
): Promise<WebhookMessage> {
  const invalid = (reason: string) => new MountableError(null, "invalid_webhook", reason);
  const header = (name: string): string => {
    if (headers instanceof Headers) {
      return headers.get(name) ?? "";
    }
    const found = Object.entries(headers).find(([key]) => key.toLowerCase() === name)?.[1];
    return Array.isArray(found) ? found.join(", ") : (found ?? "");
  };
  const id = header("webhook-id");
  const timestamp = header("webhook-timestamp");
  if (!id || !/^[0-9]+$/.test(timestamp)) {
    throw invalid("webhook-id or webhook-timestamp is missing");
  }
  if (Math.abs(Date.now() / 1000 - Number(timestamp)) > WEBHOOK_TOLERANCE) {
    throw invalid("webhook-timestamp is more than five minutes from now");
  }
  const key = secret.startsWith("whsec_") ? fromBase64(secret.slice("whsec_".length)) : null;
  if (!key || key.length === 0) {
    throw invalid("the secret is not a whsec_ secret");
  }
  const data = typeof body === "string" ? new TextEncoder().encode(body) : body;
  const signed = concat([new TextEncoder().encode(`${id}.${timestamp}.`), data]);
  const { subtle } = await webCrypto();
  const hmacKey = await subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
  let matched = false;
  for (const signature of header("webhook-signature").split(/\s+/)) {
    const given = signature.startsWith("v1,") ? fromBase64(signature.slice(3)) : null;
    // subtle.verify compares in constant time.
    if (given && (await subtle.verify("HMAC", hmacKey, given, signed))) {
      matched = true;
      break;
    }
  }
  if (!matched) {
    throw invalid("no signature matches");
  }
  try {
    const message = JSON.parse(new TextDecoder().decode(data));
    const payload = message.data;
    if (
      typeof message.type !== "string" ||
      typeof message.timestamp !== "string" ||
      typeof payload?.webhook_id !== "string" ||
      typeof payload.filesystem_id !== "string" ||
      typeof payload.cursor !== "string" ||
      !Array.isArray(payload.events)
    ) {
      throw new Error("not a message");
    }
    const sent = new Date(message.timestamp);
    if (Number.isNaN(sent.getTime())) {
      throw new Error("not a timestamp");
    }
    return {
      id,
      type: message.type,
      timestamp: sent,
      webhookId: payload.webhook_id,
      filesystemId: payload.filesystem_id,
      events: payload.events.map(change),
      cursor: payload.cursor,
    };
  } catch {
    throw invalid("the body is not a mountable.io webhook message");
  }
}

/** Standard base64 decoded, or null when it is not base64. */
function fromBase64(text: string): Uint8Array | null {
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(text) || text.length % 4 !== 0) {
    return null;
  }
  return Uint8Array.from(atob(text), (c) => c.charCodeAt(0));
}

function concat(chunks: Uint8Array[]): Uint8Array {
  const data = new Uint8Array(chunks.reduce((size, chunk) => size + chunk.length, 0));
  let offset = 0;
  for (const chunk of chunks) {
    data.set(chunk, offset);
    offset += chunk.length;
  }
  return data;
}

/** An environment variable; undefined where there is none or reading it is not allowed (Deno without --allow-env). */
function env(name: string): string | undefined {
  try {
    return globalThis.process?.env?.[name];
  } catch {
    return undefined;
  }
}

function trimSlashes(path: string): string {
  return path.replace(/^\/+|\/+$/g, "");
}

/** Percent-encodes everything but A–Z a–z 0–9 - _ . ~, as Python's quote(safe=""). */
function encodeSegment(segment: string): string {
  return encodeURIComponent(segment).replace(/[!'()*]/g, (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`);
}

function quoteEtag(etag: string): string {
  return etag.startsWith('"') || etag.startsWith("W/") ? etag : `"${etag}"`;
}

function describe(error: unknown): string {
  if (error instanceof Error) {
    const cause = error.cause instanceof Error ? `: ${error.cause.message}` : "";
    return `${error.message}${cause}`;
  }
  return String(error);
}

function change(raw: any): Change {
  return { type: raw.type, path: raw.path, file: raw.file ? fileInfo(raw.file) : null };
}

function optionalDate(value: string | null | undefined): Date | null {
  return value ? new Date(value) : null;
}

function fileInfo(raw: any): FileInfo {
  return {
    path: raw.path,
    type: raw.type,
    size: raw.size,
    etag: raw.etag ?? null,
    modifiedAt: new Date(raw.modified_at),
  };
}

function json(response: Answer): any {
  try {
    return JSON.parse(new TextDecoder().decode(response.data));
  } catch {
    throw new MountableError(response.status, "api_error", "the API's answer is not JSON");
  }
}

/**
 * Reads the API's error forms: {"detail": {"code": …}}, the same fields at
 * the top level, and {"detail": [validation errors]}.
 */
function apiError(response: Answer): MountableError {
  const status = response.status;
  let body: any = null;
  try {
    body = JSON.parse(new TextDecoder().decode(response.data));
  } catch {
    // not JSON
  }
  const isObject = (value: unknown): value is Record<string, unknown> =>
    typeof value === "object" && value !== null && !Array.isArray(value);
  let fields: Record<string, unknown> = {};
  if (isObject(body)) {
    fields = isObject(body.detail) ? body.detail : body;
  }
  let code = fields.code;
  let message = fields.message;
  if (status === 422 && isObject(body) && Array.isArray(body.detail) && body.detail.length > 0) {
    const first = body.detail[0] ?? {};
    code = "invalid_request";
    message = `${(first.loc ?? []).map(String).join(".")}: ${first.msg ?? ""}`;
  }
  if (typeof code !== "string" || !code) {
    code = "api_error";
  }
  if (typeof message !== "string" || !message) {
    message = `the API answered HTTP ${status} (${code})`;
  }
  return new MountableError(status, code as string, message as string, {
    currentEtag: typeof fields.current_etag === "string" ? fields.current_etag : null,
    retryAfter: retryAfter(response, fields),
  });
}

function retryAfter(response: Answer, fields: Record<string, unknown>): number | null {
  const header = response.headers.get("Retry-After");
  if (header !== null && /^\s*\d+\s*$/.test(header)) {
    return Number(header);
  }
  return typeof fields.retry_after === "number" ? fields.retry_after : null;
}
