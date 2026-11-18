# Pieces runner

Runs [Activepieces](https://www.activepieces.com/pieces) piece actions for
apex-engine, so a piece such as Slack, GitHub or Google Sheets can be a tool in a
plan. It loads the piece packages as libraries. It is not the Activepieces flow
engine, and it does not run triggers.

## Start it

With the Docker stack:

```bash
./apex pieces
```

On its own (Node 20 or later):

```bash
cd pieces && npm install --omit=dev
PIECES_TOKEN=$(openssl rand -hex 24) node server.mjs
```

Then give the engine `APEX_PIECES_URL=http://127.0.0.1:7391` and the same value
as `APEX_PIECES_TOKEN`.

## Use a piece

```bash
apexctl tools pieces slack                       # what is installed
apexctl tools add "Team Slack" --piece slack     # its actions become tools
apexctl tools auth <id> --piece @slack-token.txt # an API key or a JSON connection
```

For a piece that signs in with OAuth, add it without a connection and run a
consent with the piece's own endpoints:

```bash
apexctl tools oauth <id> --auth-url https://slack.com/oauth/v2/authorize \
  --token-url https://slack.com/api/oauth.v2.access \
  --client-id ... --client-secret env:SLACK_CLIENT_SECRET --scope "chat:write channels:read"
```

`GET /pieces/<name>` on the runner lists each piece's `authUrl`, `tokenUrl` and
scopes. The engine keeps the token fresh and hands the piece an `access_token`
on every call.

## Which pieces

The ones in `package.json`: 36 common ones to start with. Add a line and
reinstall, or build the image with more:

```bash
docker build --build-arg EXTRA_PIECES="xero quickbooks" -t apex-pieces pieces/
```

The names are the npm packages without the `@activepieces/piece-` prefix.

## What it does not do

- **Triggers.** A piece's webhooks and polling triggers are not run. Start
  workflows with the engine's own webhook, schedule or API.
- **Dropdowns filled in from the API.** A field that Activepieces shows as a
  list of your channels or sheets is a plain text field here and takes the id.
- **The raw "custom API call" action** every piece carries is left out; the
  engine has `tool.http` and OpenAPI import for that.
- **Files.** An action that returns a file writes it to a temporary directory
  that is removed when the action ends.
- **Platform services.** Pausing a run, waitpoints, the flow list and agent
  tools belong to the Activepieces platform and fail with a message that says so.

## Security

Piece packages are third-party code, and this service hands them credentials.

- It publishes no port in the Compose stack; only the engine can reach it.
- Set `PIECES_TOKEN`. Without it anyone who can reach the port can run actions.
- Outbound calls made by a piece do not go through the engine's SSRF guard.
  Run it where it cannot reach things it should not.
- Install only pieces you trust, and pin versions in `package.json` for
  production; `*` takes whatever is newest at build time.
- The engine sends the connection with each call and the runner keeps nothing
  between calls.

## Settings

| Variable | Default | |
|---|---|---|
| `PIECES_PORT` | `7391` | |
| `PIECES_HOST` | `127.0.0.1` | The image sets `0.0.0.0` |
| `PIECES_TOKEN` | none | Shared secret the engine must present |
| `PIECES_DIR` | this directory | Where `node_modules/@activepieces` is |
| `PIECES_RUN_TIMEOUT_MS` | `150000` | Time allowed for one action |
