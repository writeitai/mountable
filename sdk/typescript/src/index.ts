// The Mountable files API and mount tickets over HTTPS. The behaviour is the
// contract in sdk/contract.md, shared with the Python SDK.

const DEFAULT_BASE_URL = "https://mountable.io";
// Milliseconds a request may take; a change-feed poll may take its wait longer.
const TIMEOUT_MS = 60_000;
// A token session lasts an hour and is replaced this close to its expiry.
const SESSION_SECONDS = 3600;
const REFRESH_BEFORE_MS = 60_000;

export type Mode = "ro" | "rw";
export type FileType = "file" | "directory" | "symlink";
export type ChangeType = "created" | "updated" | "deleted";

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

export interface MountTicket {
  id: string;
  filesystemId: string;
  mode: Mode;
  state: string;
  ticket: string | null;
  ticketExpiresAt: Date | null;
  expiresAt: Date;
}

export interface MountableOptions {
  /** Defaults to MOUNTABLE_API_KEY. */
  apiKey?: string;
  /** Defaults to MOUNTABLE_URL, then https://mountable.io. */
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

// Every secret the API issues starts with "mtbl": API keys (mtbl_), session
// tokens (mtblst_), tickets (mtbltk_) and login tokens.
const SECRET = /(mtbl[a-z]*)_[A-Za-z0-9_.-]+/g;

function redact(text: string): string {
  return text.replace(SECRET, "$1_[redacted]");
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

  constructor(options: MountableOptions = {}) {
    const apiKey = options.apiKey || env("MOUNTABLE_API_KEY");
    if (!apiKey) {
      throw new MountableError(null, "unauthenticated", "pass apiKey or set MOUNTABLE_API_KEY");
    }
    this.#apiKey = apiKey;
    this.baseUrl = (options.baseUrl || env("MOUNTABLE_URL") || DEFAULT_BASE_URL).replace(/\/+$/, "");
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
    const session = await this.#postSession(body);
    return {
      id: session.id,
      filesystemId: session.filesystem_id,
      mode: session.mode,
      state: session.state,
      ticket: session.ticket ?? null,
      ticketExpiresAt: session.ticket_expires_at ? new Date(session.ticket_expires_at) : null,
      expiresAt: new Date(session.expires_at),
    };
  }

  async #postSession(body: Record<string, unknown>): Promise<any> {
    const response = await this.#send("POST", "/api/v1/mount-sessions", this.#apiKey, {
      body: JSON.stringify(body),
      headers: { "Content-Type": "application/json" },
    });
    if (response.status >= 300) {
      throw apiError(response);
    }
    return json(response);
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
    return {
          events: body.events.map((event: any) => ({
        type: event.type,
        path: event.path,
        file: event.file ? fileInfo(event.file) : null,
      })),
      cursor: body.cursor,
    };
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

  /** Sends one call; a read is sent once more after a network error. */
  async #request(method: string, path: string, retry: boolean, options?: SendOptions): Promise<Answer> {
    try {
      return await this.#authorized(method, path, options);
    } catch (error) {
      if (!retry || !(error instanceof MountableError) || error.code !== "network_error") {
        throw error;
      }
    }
    return this.#authorized(method, path, options);
  }

  async #authorized(method: string, path: string, options?: SendOptions): Promise<Answer> {
    const session = await this.#token();
    let response = await this.#send(method, path, session.token, options);
    if (response.status === 401) {
      if (this.#session === session) {
        this.#session = null;
      }
      response = await this.#send(method, path, (await this.#token()).token, options);
    }
    if (response.status >= 300) {
      throw apiError(response);
    }
    return response;
  }
}

type RandomSource = { getRandomValues(bytes: Uint8Array): Uint8Array };

async function randomKey(): Promise<string> {
  const bytes = new Uint8Array(16);
  // Node 18 has no global Web Crypto in ES modules.
  const source: RandomSource =
    (globalThis as { crypto?: RandomSource }).crypto ?? (await import("node:crypto")).webcrypto;
  source.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
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
