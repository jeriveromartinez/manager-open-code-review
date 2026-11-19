#!/bin/sh
# Fake `opencode run` used by the review package tests.
#
# The real command line is arbitrary; the fake selects a behaviour by looking
# for one of these sentinel arguments in the prompt:
#
#   __FAKE_ENV__              emit a JSONL text event with QA_LLM_API_KEY/HOME
#   __FAKE_EVENTS__           emit testdata/opencode-events.jsonl verbatim
#   __FAKE_ARGS__             emit a JSONL text event with the received argv
#   __FAKE_FAIL__             write to stderr and exit 1
#   __FAKE_SLEEP__=<path>     spawn a long-lived child, record its PID, wait
#
# Any unrecognised invocation behaves like __FAKE_EVENTS__.

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
fixture="$script_dir/opencode-events.jsonl"

mode=""
pidfile=""

json_escape() {
    # Enough for the simple ASCII values used by the tests.
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

text_event() {
    printf '{"type":"text","part":{"type":"text","text":"%s"}}\n' "$(json_escape "$1")"
}

for arg in "$@"; do
    case "$arg" in
        __FAKE_ENV__) mode=env ;;
        __FAKE_EVENTS__) mode=events ;;
        __FAKE_ARGS__) mode=args ;;
        __FAKE_FAIL__) mode=fail ;;
        __FAKE_SLEEP__=*) mode=sleep; pidfile=${arg#__FAKE_SLEEP__=} ;;
    esac
done

case "$mode" in
    env)
        text_event "QA_LLM_API_KEY=${QA_LLM_API_KEY-} HOME=${HOME-}"
        ;;
    args)
        text_event "$*"
        ;;
    fail)
        printf 'fake opencode: provider unavailable\n' >&2
        exit 1
        ;;
    sleep)
        # The child inherits the process group of this shell, so killing the
        # group must also terminate it (no orphan survives the timeout).
        sleep 60 &
        child=$!
        printf '%s\n' "$child" > "$pidfile"
        wait "$child"
        ;;
    *)
        cat "$fixture"
        ;;
esac
