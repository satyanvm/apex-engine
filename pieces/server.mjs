// The apex-engine pieces runner.
//
// It loads Activepieces piece packages as libraries and runs one action at a
// time for the engine. It does not run the Activepieces flow engine, and it
// does not run triggers.
//
//   GET  /healthz
//   GET  /pieces            every installed piece, without its actions
//   GET  /pieces/<name>     one piece with its actions and inputs
//   POST /execute           {piece, action, props, auth} -> {ok, output} | {ok:false, error}
//
// Piece code is third-party code that is handed credentials. Keep this service
// on a private network, set PIECES_TOKEN, and install only pieces you trust.
import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const PIECES_DIR = path.resolve(process.env.PIECES_DIR || HERE);
const PORT = Number(process.env.PIECES_PORT || 7391);
const HOST = process.env.PIECES_HOST || '127.0.0.1';
const TOKEN = process.env.PIECES_TOKEN || '';
const RUN_TIMEOUT_MS = Number(process.env.PIECES_RUN_TIMEOUT_MS || 150_000);
const MAX_BODY = 4 << 20;
const PREFIX = 'piece-';

const requireFrom = createRequire(path.join(PIECES_DIR, 'package.json'));
const scopeDir = path.join(PIECES_DIR, 'node_modules', '@activepieces');

/* --------------------------------- registry -------------------------------- */

const loaded = new Map(); // name -> piece | Error

function installed() {
  if (!fs.existsSync(scopeDir)) return [];
  return fs.readdirSync(scopeDir).filter((d) => d.startsWith(PREFIX)).sort();
}

function validName(name) {
  return /^[a-z0-9][a-z0-9-]*$/.test(name);
}

function title(name) {
  return name.split('-').map((w) => w.charAt(0).toUpperCase() + w.slice(1)).join(' ');
}

// A package exports its Piece under a name that varies, and the framework class
// differs between packages, so the piece is recognised by its shape.
function looksLikePiece(v) {
  return typeof v === 'object' && v !== null && typeof v.displayName === 'string' && typeof v.actions === 'function';
}

function load(name) {
  if (loaded.has(name)) {
    const hit = loaded.get(name);
    if (hit instanceof Error) throw hit;
    return hit;
  }
  if (!validName(name) || !fs.existsSync(path.join(scopeDir, PREFIX + name))) {
    const err = new Error(`no piece named "${name}" is installed on the pieces runner`);
    err.status = 404;
    throw err;
  }
  try {
    const dir = path.join(scopeDir, PREFIX + name);
    const mod = requireFrom(dir);
    const instance = [mod.default, ...Object.values(mod)].find(looksLikePiece);
    if (!instance) throw new Error('the package exports no piece');
    const pkg = JSON.parse(fs.readFileSync(path.join(dir, 'package.json'), 'utf8'));
    const auth = Array.isArray(instance.auth) ? instance.auth[0] : instance.auth;
    const piece = {
      name,
      package: '@activepieces/' + PREFIX + name,
      version: String(pkg.version ?? '0.0.0'),
      displayName: instance.displayName,
      description: String(instance.description ?? ''),
      categories: Array.isArray(instance.categories) ? instance.categories.map(String) : [],
      auth,
      actions: instance.actions(),
    };
    loaded.set(name, piece);
    return piece;
  } catch (cause) {
    const err = new Error(`piece "${name}" failed to load: ${cause?.message ?? cause}`);
    err.status = 422;
    loaded.set(name, err);
    throw err;
  }
}

/* --------------------------------- metadata -------------------------------- */

function describeProps(defs) {
  const out = [];
  for (const [name, def] of Object.entries(defs ?? {})) {
    if (!def || typeof def !== 'object') continue;
    const prop = {
      name,
      displayName: String(def.displayName ?? name),
      description: String(def.description ?? ''),
      type: String(def.type ?? 'SHORT_TEXT').toUpperCase(),
      required: Boolean(def.required),
    };
    const raw = Array.isArray(def.options?.options) ? def.options.options : null;
    if (raw && (prop.type === 'STATIC_DROPDOWN' || prop.type === 'STATIC_MULTI_SELECT_DROPDOWN')) {
      const options = raw
        .map((o) => (o && typeof o === 'object' && 'value' in o ? { label: String(o.label ?? o.value), value: o.value } : { label: String(o), value: o }))
        .filter((o) => ['string', 'number', 'boolean'].includes(typeof o.value));
      if (options.length === raw.length) prop.options = options;
    }
    out.push(prop);
  }
  return out;
}

function describeAuth(auth) {
  const type = String(auth?.type ?? 'NONE').toUpperCase();
  const out = { type, description: String(auth?.description ?? ''), props: [] };
  if (type === 'CUSTOM_AUTH' || type === 'OAUTH2') out.props = describeProps(auth.props);
  if (type === 'OAUTH2') {
    out.authUrl = String(auth.authUrl ?? '');
    out.tokenUrl = String(auth.tokenUrl ?? '');
    out.scope = Array.isArray(auth.scope) ? auth.scope.map(String) : [];
  }
  return out;
}

function describe(piece) {
  const actions = Object.entries(piece.actions)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, a]) => ({
      name,
      displayName: String(a.displayName ?? name),
      description: String(a.description ?? ''),
      props: describeProps(a.props),
    }));
  return {
    name: piece.name, package: piece.package, version: piece.version, displayName: piece.displayName,
    description: piece.description, categories: piece.categories, auth: describeAuth(piece.auth),
    actions, actionCount: actions.length,
  };
}

// The list does not load any piece: with hundreds installed that would take
// minutes and a great deal of memory. Names and versions come from disk.
function list() {
  return installed().map((dir) => {
    const name = dir.slice(PREFIX.length);
    let version = '';
    try {
      version = String(JSON.parse(fs.readFileSync(path.join(scopeDir, dir, 'package.json'), 'utf8')).version ?? '');
    } catch { /* an unreadable package still shows up, and fails clearly when loaded */ }
    const hit = loaded.get(name);
    const known = hit && !(hit instanceof Error) ? hit : null;
    return {
      name, package: '@activepieces/' + dir, version,
      displayName: known ? known.displayName : title(name),
      description: known ? known.description : '',
      categories: known ? known.categories : [],
      actionCount: known ? Object.keys(known.actions).length : 0,
    };
  });
}

/* ---------------------------------- running -------------------------------- */

// Pieces were compiled against different framework generations, which read a
// secret both as the bare string and as auth.secret_text, and custom fields
// both as auth.x and auth.props.x. These shapes satisfy both.
function shapeAuth(value, piece) {
  const type = String(piece.auth?.type ?? 'NONE').toUpperCase();
  if (type === 'NONE') return undefined;
  if (value === undefined || value === null || value === '') return undefined;
  switch (type) {
    case 'SECRET_TEXT': {
      const text = typeof value === 'object' ? String(value.secret_text ?? value.value ?? '') : String(value);
      if (!text) return undefined;
      // eslint-disable-next-line no-new-wrappers
      const dual = new String(text);
      dual.secret_text = text;
      dual.type = 'SECRET_TEXT';
      return dual;
    }
    case 'BASIC_AUTH': {
      if (typeof value === 'object') return { username: String(value.username ?? ''), password: String(value.password ?? '') };
      const i = String(value).indexOf(':');
      return i > 0 ? { username: String(value).slice(0, i), password: String(value).slice(i + 1) } : { username: String(value), password: '' };
    }
    case 'CUSTOM_AUTH':
      return typeof value === 'object' ? { ...value, props: value.props ?? value } : value;
    case 'OAUTH2':
      return typeof value === 'object' ? { token_type: 'Bearer', data: {}, ...value } : { access_token: String(value), token_type: 'Bearer', data: {} };
    default:
      return value;
  }
}

function withDefaults(props, defs) {
  const out = { ...props };
  for (const [key, def] of Object.entries(defs ?? {})) {
    if (out[key] === undefined && def && def.defaultValue !== undefined) out[key] = def.defaultValue;
  }
  return out;
}

function jsonSafe(v) {
  if (v === undefined) return null;
  try {
    return JSON.parse(JSON.stringify(v, (_k, val) => (typeof val === 'bigint' ? val.toString() : val)));
  } catch {
    return String(v);
  }
}

function timeout(promise, ms) {
  let timer;
  const late = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`the action took longer than ${ms / 1000}s`)), ms);
  });
  return Promise.race([promise, late]).finally(() => clearTimeout(timer));
}

async function run(piece, action, props, auth, runInfo) {
  const unsupported = (feature) => new Error(`${feature} needs the Activepieces platform, which this runner does not provide (piece ${piece.name})`);
  // A store that lasts for this one call: enough for an action that keeps
  // intermediates, and deliberately not shared between runs or tenants.
  const kv = new Map();
  const filesDir = await fs.promises.mkdtemp(path.join(os.tmpdir(), 'apex-piece-'));
  const runId = String(runInfo?.id ?? 'apex-run');
  const context = {
    executionType: 'BEGIN',
    auth,
    propsValue: withDefaults(props, action.props),
    store: {
      put: async (key, value) => (kv.set(key, value), value),
      get: async (key) => (kv.has(key) ? kv.get(key) : null),
      delete: async (key) => void kv.delete(key),
    },
    files: {
      write: async ({ fileName, data }) => {
        const p = path.join(filesDir, path.basename(fileName));
        await fs.promises.writeFile(p, data);
        return `file://${p}`;
      },
    },
    project: { id: 'apex', externalId: async () => undefined },
    connections: { get: async () => null },
    tags: { add: async () => undefined },
    server: { apiUrl: 'http://unsupported.invalid/', publicUrl: 'http://unsupported.invalid/', token: '' },
    flows: { list: async () => { throw unsupported('flows.list'); }, current: { id: runId, version: { id: 'v1' } } },
    step: { name: String(runInfo?.node ?? action.name) },
    output: { update: async () => undefined },
    agent: { tools: async () => { throw unsupported('agent tools'); } },
    run: {
      id: runId,
      stop: () => undefined,
      respond: () => undefined,
      pause: () => { throw unsupported('pausing a run'); },
      createWaitpoint: async () => { throw unsupported('waitpoints'); },
      waitForWaitpoint: () => { throw unsupported('waitpoints'); },
    },
    generateResumeUrl: () => { throw unsupported('resume URLs'); },
    serverUrl: 'http://unsupported.invalid/',
  };
  try {
    return await timeout(Promise.resolve().then(() => action.run(context)), RUN_TIMEOUT_MS);
  } finally {
    fs.promises.rm(filesDir, { recursive: true, force: true }).catch(() => {});
  }
}

// A failed HTTP call inside a piece carries the provider's answer; without it
// the error is only "Request failed with status code 400".
function explain(err) {
  let msg = err?.message ? String(err.message) : String(err);
  const body = err?.response?.body ?? err?.response?.data;
  if (body !== undefined) {
    const text = typeof body === 'string' ? body : JSON.stringify(jsonSafe(body));
    if (text && !msg.includes(text.slice(0, 40))) msg += ': ' + text.slice(0, 500);
  }
  return msg.slice(0, 1500);
}

async function execute(body) {
  const name = String(body.piece ?? '').replace(/^@activepieces\/piece-/, '');
  const piece = load(name);
  const action = piece.actions[String(body.action ?? '')];
  if (!action) {
    const err = new Error(`piece "${name}" has no action "${body.action}"`);
    err.status = 404;
    throw err;
  }
  const auth = shapeAuth(body.auth, piece);
  const type = String(piece.auth?.type ?? 'NONE').toUpperCase();
  if (auth === undefined && type !== 'NONE' && action.requireAuth !== false && piece.auth?.required !== false) {
    return { ok: false, error: `${piece.displayName} needs a connection (${type}); set it on this tool with PUT /v1/catalogs/{id}/credentials` };
  }
  try {
    const output = await run(piece, action, body.props && typeof body.props === 'object' ? body.props : {}, auth, body.run);
    return { ok: true, output: jsonSafe(output) };
  } catch (err) {
    return { ok: false, error: explain(err) };
  }
}

/* ----------------------------------- server -------------------------------- */

function send(res, status, value) {
  const raw = JSON.stringify(value);
  res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(raw) });
  res.end(raw);
}

function readJSON(req) {
  return new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];
    req.on('data', (c) => {
      size += c.length;
      if (size > MAX_BODY) {
        reject(Object.assign(new Error('request body is larger than 4 MB'), { status: 413 }));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => {
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}'));
      } catch {
        reject(Object.assign(new Error('request body is not JSON'), { status: 400 }));
      }
    });
    req.on('error', reject);
  });
}

function sameToken(given) {
  if (given.length !== TOKEN.length) return false;
  let diff = 0;
  for (let i = 0; i < given.length; i++) diff |= given.charCodeAt(i) ^ TOKEN.charCodeAt(i);
  return diff === 0;
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, 'http://runner');
    if (req.method === 'GET' && url.pathname === '/healthz') {
      return send(res, 200, { status: 'ok', installed: installed().length });
    }
    if (TOKEN && !sameToken((req.headers.authorization || '').replace(/^Bearer /, ''))) {
      return send(res, 401, { ok: false, error: 'missing or invalid pieces token' });
    }
    if (req.method === 'GET' && url.pathname === '/pieces') {
      return send(res, 200, { pieces: list() });
    }
    if (req.method === 'GET' && url.pathname.startsWith('/pieces/')) {
      return send(res, 200, describe(load(decodeURIComponent(url.pathname.slice('/pieces/'.length)))));
    }
    if (req.method === 'POST' && url.pathname === '/execute') {
      return send(res, 200, await execute(await readJSON(req)));
    }
    send(res, 404, { ok: false, error: 'not found' });
  } catch (err) {
    send(res, err?.status ?? 500, { ok: false, error: err?.message ? String(err.message) : String(err) });
  }
});

// One action that rejects a promise nobody awaits must not take the runner,
// and every other run in flight, down with it.
process.on('unhandledRejection', (reason) => console.error('unhandled rejection in a piece:', reason));
process.on('uncaughtException', (err) => console.error('uncaught exception in a piece:', err));

server.listen(PORT, HOST, () => {
  console.log(JSON.stringify({ msg: 'pieces runner listening', host: HOST, port: PORT, installed: installed().length, token: Boolean(TOKEN) }));
});
