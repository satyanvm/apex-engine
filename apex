#!/usr/bin/env bash
# apex: start and manage a local apex-engine. Docker is the only requirement.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

ENV_FILE=.env
PROFILE=(--profile demo)
# ./apex pieces records APEX_PIECES=1, and from then on the runner is part of the stack.
if [ -f "$ENV_FILE" ] && [ "$(sed -n 's/^APEX_PIECES=//p' "$ENV_FILE" | tail -n 1)" = 1 ]; then
  PROFILE+=(--profile pieces)
fi

say()  { printf '%s\n' "$*"; }
die()  { printf 'apex: %s\n' "$*" >&2; exit 1; }

env_get() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1
}

# env_set replaces or appends one line and never passes the value through a
# regular expression, so secrets with special characters are safe.
env_set() {
  local key=$1 value=$2 tmp
  tmp=$(mktemp "$ENV_FILE.XXXXXX")
  { [ -f "$ENV_FILE" ] && grep -v "^$key=" "$ENV_FILE" || true; printf '%s=%s\n' "$key" "$value"; } > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$ENV_FILE"
}

random_hex() { od -An -tx1 -N "$1" /dev/urandom | tr -d ' \n'; }
random_b64() { head -c "$1" /dev/urandom | base64 | tr -d '\n'; }

need_docker() {
  command -v docker >/dev/null 2>&1 || die "Docker is required. Get it from https://docs.docker.com/get-docker/"
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required. Update Docker."
  docker info >/dev/null 2>&1 || die "Docker is installed but not running. Start it and try again."
}

compose() { docker compose "${PROFILE[@]}" "$@"; }

port()    { local p; p=$(env_get APEX_PORT); printf '%s' "${p:-8700}"; }
api_url() { printf 'http://localhost:%s' "$(port)"; }
api_key() { env_get APEX_API_KEYS | cut -d: -f1; }

ensure_env() {
  [ -f "$ENV_FILE" ] && return 0
  say "Creating .env with fresh secrets."
  umask 077
  : > "$ENV_FILE"
  env_set POSTGRES_PASSWORD "$(random_hex 24)"
  env_set APEX_SECRET_KEY "$(random_b64 32)"
  env_set APEX_API_KEYS "$(random_hex 24):local"
  env_set APEX_DEMO_URL "http://demo:8701"
  env_set APEX_METRICS_TOKEN "$(random_hex 16)"
  # Lets workflows call the sample tools and services on this computer. The
  # engine is only published on 127.0.0.1, and production turns this off.
  env_set APEX_ALLOW_PRIVATE_NETWORK 1
}

model_configured() {
  [ -n "$(env_get GEMINI_API_KEY)$(env_get ANTHROPIC_API_KEY)$(env_get OPENAI_API_KEY)$(env_get PLANNER_MODEL)" ]
}

# Adopt a key already in the shell, so `GEMINI_API_KEY=... ./apex up` just works.
adopt_shell_keys() {
  local name
  for name in GEMINI_API_KEY ANTHROPIC_API_KEY OPENAI_API_KEY; do
    if [ -n "${!name:-}" ] && [ -z "$(env_get "$name")" ]; then
      env_set "$name" "${!name}"
      say "Using $name from your shell."
    fi
  done
}

ollama_first_chat_model() {
  curl -sf -m 2 http://localhost:11434/api/tags 2>/dev/null | tr '{' '\n' |
    sed -n 's/^"name":"\([^"]*\)".*/\1/p' | grep -v embed | head -n 1 || true
}

read_secret() {
  local prompt=$1 value
  IFS= read -rsp "$prompt" value
  printf '\n' >&2
  printf '%s' "$value"
}

clear_models() {
  local name
  for name in GEMINI_API_KEY ANTHROPIC_API_KEY OPENAI_API_KEY OPENAI_BASE_URL PLANNER_MODEL PLANNER_MAP_MODEL OLLAMA_HOST; do
    if grep -q "^$name=" "$ENV_FILE" 2>/dev/null; then
      env_set "$name" ""
    fi
  done
}

choose_model() {
  local local_model choice key
  local_model=$(ollama_first_chat_model)
  say
  say "apex-engine needs a model to turn sentences into plans. Which one?"
  say "  1) Google Gemini     (has a free tier: https://aistudio.google.com/apikey)"
  say "  2) Anthropic Claude  (https://console.anthropic.com)"
  say "  3) OpenAI            (https://platform.openai.com/api-keys)"
  [ -n "$local_model" ] && say "  4) Ollama on this computer ($local_model, free and private)"
  say "  s) Skip for now      (the sample workflow still runs)"
  read -rp "Choose: " choice
  case "$choice" in
    1) key=$(read_secret "Gemini API key: ");    [ -n "$key" ] || die "no key entered"; clear_models; env_set GEMINI_API_KEY "$key" ;;
    2) key=$(read_secret "Anthropic API key: "); [ -n "$key" ] || die "no key entered"; clear_models; env_set ANTHROPIC_API_KEY "$key" ;;
    3) key=$(read_secret "OpenAI API key: ");    [ -n "$key" ] || die "no key entered"; clear_models; env_set OPENAI_API_KEY "$key" ;;
    4) [ -n "$local_model" ] || die "Ollama was not found"
       clear_models
       env_set PLANNER_MODEL "ollama/$local_model"
       env_set PLANNER_MAP_MODEL "ollama/$local_model"
       env_set OLLAMA_HOST "http://host.docker.internal:11434" ;;
    *) say "Skipping. Run ./apex model when you want to compile prompts." ;;
  esac
}

wait_healthy() {
  local url i
  url="$(api_url)/healthz"
  printf 'Waiting for the engine'
  for i in $(seq 1 150); do
    if curl -sf -m 2 "$url" >/dev/null 2>&1; then
      printf ' ready.\n'
      return 0
    fi
    printf '.'
    sleep 2
  done
  printf '\n'
  docker compose "${PROFILE[@]}" logs --tail 30 engine >&2 || true
  die "the engine did not become healthy. The log above should say why."
}

open_browser() {
  local url="$(api_url)/#key=$(api_key)"
  if command -v open >/dev/null 2>&1; then open "$url"
  elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$url" >/dev/null 2>&1
  else say "Open this in your browser: $url"; fi
}

cmd_up() {
  need_docker
  ensure_env
  adopt_shell_keys
  if ! model_configured && [ -t 0 ]; then choose_model; fi
  compose up -d --build
  wait_healthy
  say
  say "apex-engine is running. Point your code or apexctl at it:"
  say
  say "  export APEX_URL=$(api_url) APEX_API_KEY=$(api_key)"
  say "  go run ./cmd/apexctl demo"
  say
  say "API contract: $(api_url)/openapi.yaml"
  say "Stop it with ./apex down. Your data stays until ./apex reset."
}

cmd_console() {
  need_docker
  ensure_env
  if [ "$(env_get APEX_CONSOLE)" != 1 ]; then
    env_set APEX_CONSOLE 1
    compose up -d --no-deps engine
    wait_healthy
  fi
  open_browser
}

# Adds the Activepieces runner, so pieces such as Slack or GitHub can be tools.
cmd_pieces() {
  need_docker
  ensure_env
  if [ "$(env_get APEX_PIECES)" != 1 ]; then
    env_set APEX_PIECES 1
    env_set APEX_PIECES_URL "http://pieces:7391"
    [ -n "$(env_get APEX_PIECES_TOKEN)" ] || env_set APEX_PIECES_TOKEN "$(random_hex 24)"
    PROFILE+=(--profile pieces)
  fi
  say "Building the pieces runner. The first build downloads the piece packages and takes a few minutes."
  compose up -d --build pieces
  compose up -d --no-deps engine
  wait_healthy
  say
  say "Pieces are available. See what can be added, then add one:"
  say
  say "  go run ./cmd/apexctl tools pieces"
  say "  go run ./cmd/apexctl tools add Slack --piece slack"
  say
  say "The list of installed pieces is pieces/package.json."
}

cmd_model() {
  need_docker
  ensure_env
  choose_model
  compose up -d --no-deps engine
  wait_healthy
}

cmd_reset() {
  local answer
  read -rp "This deletes all workflows, runs and stored credentials. Type reset to confirm: " answer
  [ "$answer" = reset ] || die "cancelled"
  compose down -v
  say "Everything is deleted. .env is kept, so your keys still work."
}

cmd_status() {
  need_docker
  compose ps
  if curl -sf -m 2 "$(api_url)/healthz" >/dev/null 2>&1; then
    say
    curl -sf -m 5 -H "Authorization: Bearer $(api_key)" "$(api_url)/v1/status" || true
    say
  fi
}

usage() {
  cat <<USAGE
Usage: ./apex <command>

  up       Build and start the engine and everything it needs
  down     Stop everything (your data is kept)
  env      Print the APEX_URL and APEX_API_KEY exports for apexctl
  console  Turn on the optional web inspector and open it
  pieces   Add the Activepieces runner, so pieces can be used as tools
  status   Show what is running and which model is connected
  logs     Follow the engine's logs
  model    Connect or change the model
  key      Print the API key
  reset    Delete all data
USAGE
}

case "${1:-}" in
  up)     cmd_up ;;
  down)   need_docker; compose down ;;
  console) cmd_console ;;
  pieces) cmd_pieces ;;
  env)    [ -f "$ENV_FILE" ] || die "not set up yet. Run ./apex up"; printf 'export APEX_URL=%s APEX_API_KEY=%s\n' "$(api_url)" "$(api_key)" ;;
  status) cmd_status ;;
  logs)   need_docker; compose logs -f --tail 100 engine ;;
  model)  cmd_model ;;
  key)    [ -f "$ENV_FILE" ] || die "not set up yet. Run ./apex up"; api_key; printf '\n' ;;
  reset)  need_docker; cmd_reset ;;
  ""|-h|--help|help) usage ;;
  *)      usage >&2; exit 2 ;;
esac
