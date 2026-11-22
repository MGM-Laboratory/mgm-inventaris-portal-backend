#!/usr/bin/env bash
# Updates the copies of the inventory portal on a host one at a time, so the other copy of each service keeps answering while one restarts.
#
# usage  ./rolling-update.sh [--dry-run] [--api <tag>] [--web <tag>]   for example ./rolling-update.sh --api 0.2.0 --web 0.1.1
#         A tag is a version such as 1.2.3, or a tag of a commit such as sha-1a2b3c4, which is what the images have until the first release.
#
# At least one of the two options is needed. A service that is not named keeps its copies and its tag as they are. A version is
# written as 1.2.3, with or without the v.
#
# What it needs on the host. Docker with the compose plugin (v2), curl and bash 3.2 or newer, so it runs on Linux and on macOS. A
# compose file with one service for each copy, api-1, api-2, web-1 and web-2 in compose.production.example.yml, listening on the
# host ports of the env file. A reverse proxy that stops sending requests to a copy that is down, because that is what keeps the
# portal up while a copy restarts. Run it in tmux or screen, so a dropped ssh connection does not stop it half way.
#
# What it does, in this order. It checks the env file and that the services exist, then pulls the images of the versions, which
# also proves that the tags exist, and only then keeps a copy of the env file as .env.bak and sets the tags in it. When the pull
# fails it changes nothing, and an interrupt before the first copy puts the env file back as it was. Then for each copy in order,
# the API copies first and the web copies after them, it recreates that one copy with no dependencies and waits until Docker reports
# the copy healthy, the published port answers and the container runs the image of the version that was asked for, twice in a row. It
# goes on to the next copy only when the one before is healthy. When a copy does not become healthy it stops, says which copies are
# already new and which are still old, and prints the command that goes back. Nothing is rolled back by itself.
#
# The health listener of the API is not published, so Docker runs the check inside the container with the command api healthcheck.
# The script reads the result from the state of the container, and it asks GET /api/config on the published port of the API and
# GET /healthz on the published port of the web app, on the address of API_BIND and WEB_BIND in the env file (the loopback address
# when they are not set), so a wrong port or address in the env file shows up here.
#
# Going back to an older API version is allowed only when ROLLBACK_OK is set to that version, because a newer version may have
# applied a database migration that the older version cannot run on. Read the changelog first, because a newer version may have
# added migrations. The web app has no migration, so going back there needs no confirmation.
#
# Settings, all optional, given as environment variables
#   COMPOSE_DIR   the folder with the compose file and the env file, default the folder of this script
#   TIMEOUT       seconds to wait for one copy to become healthy, default 120
#   ROLLBACK_OK   the API version that you confirm to go back to
#   DRY_RUN       1, true or yes prints the steps and changes nothing, but it still checks the services. The flag --dry-run does the same
set -euo pipefail

die() {
  echo "$*" >&2
  exit 1
}

usage="usage rolling-update.sh [--dry-run] [--api <tag>] [--web <tag>], for example --api 0.2.0 --web 0.1.1 or --api sha-1a2b3c4"

api_version=""
web_version=""
dry_run_flag=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --dry-run)
      dry_run_flag=1
      shift
      ;;
    --api | --web)
      [ "$#" -ge 2 ] || die "$1 needs a tag. $usage"
      [[ "$2" =~ ^(v?[0-9]+\.[0-9]+\.[0-9]+|sha-[0-9a-f]{7,40})$ ]] || die "The tag after $1 must look like 1.2.3 or sha-1a2b3c4, got $2"
      if [ "$1" = "--api" ]; then api_version="${2#v}"; else web_version="${2#v}"; fi
      shift 2
      ;;
    *) die "Unknown argument $1. $usage" ;;
  esac
done
[ -n "$api_version$web_version" ] || die "$usage"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
script="$script_dir/$(basename "${BASH_SOURCE[0]}")"
compose_dir="${COMPOSE_DIR:-$script_dir}"
# the services of the compose file load .env too, so the file name is not a setting, or the two could name different files
env_file=".env"
timeout="${TIMEOUT:-120}"

case "${DRY_RUN:-0}" in
  1 | true | yes) dry_run=1 ;;
  0 | false | no | "") dry_run=0 ;;
  *) die "DRY_RUN must be 1, true, yes, 0, false or no, got ${DRY_RUN}" ;;
esac
[ "$dry_run_flag" = "0" ] || dry_run=1

for tool in docker curl; do
  command -v "$tool" >/dev/null || die "$tool is not installed"
done

[[ "$timeout" =~ ^[0-9]+$ ]] || die "TIMEOUT must be a number of seconds, got $timeout"

cd "$compose_dir"
[ -f "$env_file" ] || die "$env_file was not found in $compose_dir"

# env_value prints the last value of a setting in the env file, without quotes or a carriage return
env_value() {
  sed -n "s/^$1=//p" "$env_file" | tail -1 | tr -d "\r\"'"
}

previous_api="$(env_value API_IMAGE_TAG)"
previous_web="$(env_value WEB_IMAGE_TAG)"

# a compose command reads the tags of both images, so both have to exist, the requested one now and the other one in the env file
if [ -z "$api_version" ] && [ -z "$previous_api" ]; then die "API_IMAGE_TAG is not set in $env_file, so give --api. Nothing was changed"; fi
if [ -z "$web_version" ] && [ -z "$previous_web" ]; then die "WEB_IMAGE_TAG is not set in $env_file, so give --web. Nothing was changed"; fi
api_target="${api_version:-$previous_api}"
web_target="${web_version:-$previous_web}"

# the host ports have no default, so a missing one stops the script before it touches anything
port_of() {
  local value
  value="$(env_value "$1")"
  [[ "$value" =~ ^[0-9]+$ ]] || die "$1 is not set to a port number in $env_file, nothing was changed"
  echo "$value"
}

# bind_of prints the address that the published ports listen on, as the probe has to ask it. The loopback address is the default, and
# the address of a wildcard bind is asked through the loopback address. Only an IPv4 address is accepted, because the compose file
# writes the bind address without brackets.
bind_of() {
  local value
  value="$(env_value "$1")"
  value="${value:-127.0.0.1}"
  [[ "$value" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "$1 in $env_file is not an IPv4 address, nothing was changed"
  case "$value" in
    0.0.0.0) value="127.0.0.1" ;;
  esac
  echo "$value"
}

services=()
components=()
hosts=()
ports=()
paths=()
# the API copies come first and the web copies after them, and a service that was not asked for is left out
if [ -n "$api_version" ]; then
  # one assignment for each port, because a failure of port_of is only seen when it is the whole assignment
  port_1="$(port_of API_PORT_1)"
  port_2="$(port_of API_PORT_2)"
  [ "$port_1" != "$port_2" ] || die "API_PORT_1 and API_PORT_2 are the same port in $env_file, nothing was changed"
  host="$(bind_of API_BIND)"
  services+=(api-1 api-2)
  components+=(api api)
  hosts+=("$host" "$host")
  ports+=("$port_1" "$port_2")
  paths+=(/api/config /api/config)
fi
if [ -n "$web_version" ]; then
  port_1="$(port_of WEB_PORT_1)"
  port_2="$(port_of WEB_PORT_2)"
  [ "$port_1" != "$port_2" ] || die "WEB_PORT_1 and WEB_PORT_2 are the same port in $env_file, nothing was changed"
  host="$(bind_of WEB_BIND)"
  services+=(web-1 web-2)
  components+=(web web)
  hosts+=("$host" "$host")
  ports+=("$port_1" "$port_2")
  paths+=(/healthz /healthz)
fi

# only one update runs at a time, because two of them would fight over the env file and the same copy. A folder is the lock,
# because mkdir is atomic on every system, and the process id inside it shows a lock that a killed update left behind
lock=".rolling-update.lock"
if ! mkdir "$lock" 2>/dev/null; then
  holder="$(cat "$lock/pid" 2>/dev/null || true)"
  if [ -z "$holder" ]; then
    # the other update made the folder and has not written its process id yet
    sleep 2
    holder="$(cat "$lock/pid" 2>/dev/null || true)"
  fi
  if [ -n "$holder" ] && kill -0 "$holder" 2>/dev/null; then
    die "Another update is running on this host, process $holder"
  fi
  echo "Removing the lock of an update that is no longer running, process ${holder:-unknown}" >&2
  rm -f "$lock/pid"
  rmdir "$lock" 2>/dev/null || die "The lock $lock could not be removed, check that no update runs and remove it by hand"
  mkdir "$lock" 2>/dev/null || die "Another update took the lock a moment ago"
fi
echo "$$" >"$lock/pid"
release_lock() {
  rm -f "$lock/pid"
  rmdir "$lock" 2>/dev/null || true
}

# The tags are set for every compose command, because a value that is exported in the shell of the operator would otherwise win over
# the env file and the copies would start on another tag than the one that was pulled and checked. The host ports are cleared for
# the same reason, because the polling reads them from the env file and must see the ports that compose publishes. The bind
# addresses are cleared for the same reason
compose() {
  env -u API_PORT_1 -u API_PORT_2 -u WEB_PORT_1 -u WEB_PORT_2 -u API_BIND -u WEB_BIND \
    API_IMAGE_TAG="$api_target" WEB_IMAGE_TAG="$web_target" docker compose --env-file "$env_file" "$@"
}

run() {
  if [ "$dry_run" = "1" ]; then
    echo "DRY RUN $*"
  else
    "$@"
  fi
}

# running_version prints the tag of the image of the container of a service, which is the version that compose started it with
running_version() {
  local id image
  id="$(compose ps -q "$1" 2>/dev/null | sed -n 1p)"
  [ -n "$id" ] || return 1
  image="$(docker inspect --format '{{.Config.Image}}' "$id" 2>/dev/null)" || return 1
  echo "${image##*:}"
}

# container_health prints the state of the health check of the container of a service, such as starting, healthy or unhealthy
container_health() {
  local id
  id="$(compose ps -q "$1" 2>/dev/null | sed -n 1p)"
  [ -n "$id" ] || return 1
  docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$id" 2>/dev/null
}

# healthy succeeds only when Docker reports the copy healthy, the published port answers 200 and the container runs the version that
# was asked for. It prints the state that it saw, so a failure shows why.
healthy() {
  local service="$1" host="$2" port="$3" path="$4" want="$5" state tag
  state="$(container_health "$service")" || state="missing"
  echo "docker=$state"
  [ "$state" = "healthy" ] || return 1
  curl -fsS --max-time 5 -o /dev/null "http://${host}:${port}${path}" 2>/dev/null || {
    echo "${host}:${port} did not answer 200"
    return 1
  }
  tag="$(running_version "$service")" || return 1
  [ "$tag" = "$want" ]
}

# older succeeds when the first version is older than the second one, both written as 1.2.3
older() {
  local IFS=.
  # shellcheck disable=SC2206
  local a=($1) b=($2)
  local i
  for i in 0 1 2; do
    if [ "${a[$i]}" -lt "${b[$i]}" ]; then return 0; fi
    if [ "${a[$i]}" -gt "${b[$i]}" ]; then return 1; fi
  done
  return 1
}

# the env file as it is now, byte for byte. The x keeps the newlines at the end, and it is cut off again
env_original="$(
  cat "$env_file"
  printf x
)"
env_original="${env_original%x}"
changed=0
updated=()
# 1 from the moment the first copy is recreated, because from then on a copy may run the new version
copy_started=0

# restore_tags puts the env file back as it was, for a step that fails before any copy is touched. Without it the env file would name
# a version that no copy runs, and a later docker compose up -d would recreate every copy on it at once
restore_tags() {
  [ "$changed" = "1" ] || return 0

  if printf '%s' "$env_original" >"$env_file"; then
    changed=0
    return 0
  fi

  echo "The tags in $env_file could not be put back, set API_IMAGE_TAG to ${previous_api:-not set} and WEB_IMAGE_TAG to ${previous_web:-not set} by hand before anything runs docker compose up -d" >&2
  return 1
}

on_signal() {
  echo >&2
  if [ "$copy_started" = "0" ]; then
    restore_tags || true
  fi
  local now="API ${previous_api:-not set} and WEB ${previous_web:-not set}"
  if [ "$changed" = "1" ]; then
    now="API ${api_target} and WEB ${web_target}"
  fi
  echo "Interrupted. The tags in $env_file are $now, copies updated so far ${updated[*]:-none}." >&2
  echo "Check the copies with docker compose ps and run the update again when they are fine." >&2
  release_lock
  exit 130
}
trap on_signal INT TERM HUP
trap release_lock EXIT

# the API refuses a missing URL and the placeholder key of the template, and a copy that cannot start never becomes healthy, so the
# script stops before it touches anything. It checks only that the values are not the fake ones of the template.
if [ -n "$api_version" ]; then
  database_url="$(env_value DATABASE_URL)"
  [ -n "$database_url" ] || die "DATABASE_URL is not set in $env_file, nothing was changed"
  [[ "$database_url" != *CHANGE_ME* ]] || die "DATABASE_URL in $env_file still holds the placeholder password, nothing was changed"
  [ "$(env_value TOTP_ENCRYPTION_KEY)" != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" ] || die "TOTP_ENCRYPTION_KEY in $env_file is the placeholder of the template, nothing was changed"
fi

echo "Checking that the services exist"
known="$(compose config --services)"
for service in "${services[@]}"; do
  grep -qxF -- "$service" <<<"$known" || die "The service $service is not in the compose file, nothing was changed. The services are $(echo "$known" | tr '\n' ' ')"
done

# going back needs a confirmation, because a newer API version may have changed the database in a way that the older one cannot run on
if [[ "$api_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] && [[ "$previous_api" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] && [ "$previous_api" != "$api_version" ] && older "$api_version" "$previous_api"; then
  echo "WARNING $api_version is older than $previous_api, which runs now. A newer version may have applied a database migration that $api_version cannot run on." >&2
  rollback_ok="${ROLLBACK_OK:-}"
  [ "${rollback_ok#v}" = "$api_version" ] || die "Going back needs ROLLBACK_OK=$api_version, nothing was changed"
fi

# compose pulls the images that the services of the compose file really use, so a compose file that names another image than the
# one of the release is caught here, before anything is changed. The tags are the ones of this command, so the env file is not
# touched yet. One service of each image is enough, because both copies use the same image.
echo "Pulling the images of the versions, which also proves that the tags exist"
pull_services=()
[ -z "$api_version" ] || pull_services+=(api-1)
[ -z "$web_version" ] || pull_services+=(web-1)
if [ "$dry_run" = "1" ]; then
  echo "DRY RUN docker compose pull ${pull_services[*]}"
else
  compose pull "${pull_services[@]}" || die "The images could not be pulled, so nothing was changed. A tag may not exist, or the registry refused the pull, or the package is private"
fi

# set_tag writes one setting of the env file in place, so a link or the mode of the file stays as it is, which sed -i does not do on
# every system
set_tag() {
  local name="$1" value="$2" new_env
  if grep -q "^${name}=" "$env_file"; then
    new_env="$(
      awk -v n="$name" -v v="$value" 'index($0, n "=") == 1 { print n "=" v; next } { print }' "$env_file"
      printf x
    )"
    printf '%s' "${new_env%x}" >"$env_file"
  else
    printf '\n%s=%s\n' "$name" "$value" >>"$env_file"
  fi
}

echo "Keeping a copy of $env_file as $env_file.bak and setting the tags"
if [ "$dry_run" = "1" ]; then
  echo "DRY RUN copy $env_file to $env_file.bak, then set API_IMAGE_TAG=${api_target} and WEB_IMAGE_TAG=${web_target}"
else
  # the copy is written from memory, so it is the file as it was before this update, byte for byte
  printf '%s' "$env_original" >"$env_file.bak"
  # set before the write, so an interrupt in the middle of it puts the file back from the copy in memory
  changed=1
  [ -z "$api_version" ] || set_tag API_IMAGE_TAG "$api_version"
  [ -z "$web_version" ] || set_tag WEB_IMAGE_TAG "$web_version"
fi

# the command that goes back, with the folder when it is not the default
go_back() {
  local prefix="" args=""
  [ -z "${COMPOSE_DIR:-}" ] || prefix="COMPOSE_DIR=${COMPOSE_DIR} "
  if [ -n "$api_version" ]; then
    [ -n "$previous_api" ] || { echo "nothing, because no earlier API version was set in $env_file. Stop the copies with docker compose stop instead"; return; }
    prefix="${prefix}ROLLBACK_OK=${previous_api} "
    args="--api ${previous_api}"
  fi
  if [ -n "$web_version" ]; then
    [ -n "$previous_web" ] || { echo "nothing, because no earlier web version was set in $env_file. Stop the copies with docker compose stop instead"; return; }
    args="${args:+$args }--web ${previous_web}"
  fi
  echo "${prefix}${script} ${args}"
}

# half_done tells the operator what runs where after a copy failed, and why a plain docker compose up -d is a mistake now
half_done() {
  local failed="$1" index="$2" rest=() j
  for ((j = index + 1; j < ${#services[@]}; j++)); do
    rest+=("${services[$j]}")
  done
  echo "$failed was recreated on ${want:-the new version} and is not healthy. The last state was ${answer:-none}." >&2
  # a database that does not answer makes every API copy unhealthy, so going back to the old version would not help
  if [ "${components[$index]}" = "api" ]; then
    echo "For an API copy the cause may be the database or a setting, and not the new version, so read the logs and check the database before going back." >&2
  fi
  echo "Already healthy on the new version ${updated[*]:-none}. Still on the old version ${rest[*]:-none}." >&2
  echo "The tags in $env_file name the new versions now, so do not run docker compose up -d before the copies are checked, because it would recreate every copy on them." >&2
  echo "Nothing was rolled back. To go back run $(go_back), and read the logs with docker compose logs $failed first." >&2
}

for i in "${!services[@]}"; do
  service="${services[$i]}"
  host="${hosts[$i]}"
  port="${ports[$i]}"
  path="${paths[$i]}"
  want="$api_version"
  [ "${components[$i]}" = "api" ] || want="$web_version"

  echo "Updating $service on ${host}:$port"
  copy_started=1
  if ! run compose up -d --no-deps --force-recreate "$service"; then
    echo "$service could not be recreated. Check docker compose ps and the logs of $service." >&2
    half_done "$service" "$i"
    exit 1
  fi

  if [ "$dry_run" = "1" ]; then
    updated+=("$service")
    continue
  fi

  ok=0
  answer=""
  start="$SECONDS"
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    if answer="$(healthy "$service" "$host" "$port" "$path" "$want")"; then
      ok=$((ok + 1))
      echo "$service healthy ($ok of 2) $answer"
      if [ "$ok" -ge 2 ]; then
        break
      fi
    else
      ok=0
      echo "$service not healthy yet after $((SECONDS - start))s, the last state was ${answer:-none}"
    fi
    sleep 2
  done

  if [ "$ok" -lt 2 ]; then
    echo "$service did not become healthy within ${timeout} seconds." >&2
    half_done "$service" "$i"
    exit 1
  fi
  updated+=("$service")
done

echo "Done. ${updated[*]} run the new versions, API ${api_target} and WEB ${web_target}."
