#!/bin/sh
# Fake Copilot CLI for tower's runner tests. It never contacts any service.
#
# Controls (environment variables, never tower configuration):
#   FAKE_COPILOT_MODE        ready (default) | blocked | invalid |
#                            missing-result | missing-report | nonzero | hang |
#                            descendant (ready plus a TERM-ignoring
#                            background descendant)
#   FAKE_COPILOT_DELAY       seconds to sleep before finishing (default 0)
#   FAKE_COPILOT_DESCENDANT  1: leave a TERM-ignoring background descendant in
#                            the process group (any mode)
#   FAKE_COPILOT_EARLY       1: write the result files before the delay
#   FAKE_COPILOT_DIR         control directory:
#                              mode.<item-id>     overrides the mode per item
#                              hold               if present at start, wait
#                                                 for release.<item-id> or
#                                                 release.all before finishing
#                              log                appends "start|end <item-id>
#                                                 <run>" lines
#
# Evidence written into $RUN: fake-argv (NUL-separated arguments),
# fake-cwd, fake-run (the RUN value), fake-probe (FAKE_COPILOT_PROBE),
# fake-pid, fake-pgid and fake-descendant.

run_dir=$RUN
item_id=$(basename "$(pwd -P)")
run_n=$(basename "$run_dir")
ctl=${FAKE_COPILOT_DIR:-}

printf '%s\0' "$@" >"$run_dir/fake-argv"
pwd -P >"$run_dir/fake-cwd"
printf '%s' "$RUN" >"$run_dir/fake-run"
printf '%s' "${FAKE_COPILOT_PROBE:-}" >"$run_dir/fake-probe"
echo $$ >"$run_dir/fake-pid"
ps -o pgid= -p $$ | tr -d ' ' >"$run_dir/fake-pgid"

mode=${FAKE_COPILOT_MODE:-ready}
if [ -n "$ctl" ] && [ -f "$ctl/mode.$item_id" ]; then
	mode=$(cat "$ctl/mode.$item_id")
fi
if [ -n "$ctl" ]; then
	echo "start $item_id $run_n" >>"$ctl/log"
fi
echo "fake copilot running" >&2

if [ "$mode" = descendant ] || [ "${FAKE_COPILOT_DESCENDANT:-}" = 1 ]; then
	(
		trap '' TERM
		while :; do sleep 1; done
	) &
	echo $! >"$run_dir/fake-descendant"
fi

write_report() {
	printf '# Report\n\nFake investigation of %s.\n' "$item_id" >"$run_dir/report.md"
}

write_result() {
	printf '%s\n' "$1" >"$run_dir/result.json"
}

ready='{"version":1,"status":"ready","summary":"Fake summary of the alert.","confidence":"high","root_cause":"fake","assumptions":[],"gate":{"question":"Create the fix?","options":["yes","no"]},"proposed_actions":[{"id":1,"type":"other","title":"Do it","description":"Fake action."}]}'
blocked='{"version":1,"status":"blocked","summary":"Fake investigation stopped.","assumptions":[],"gate":{"question":"Which cluster is affected?"},"proposed_actions":[]}'

artifacts() {
	case "$mode" in
	ready | descendant | nonzero)
		write_report
		write_result "$ready"
		;;
	blocked)
		write_report
		write_result "$blocked"
		;;
	invalid)
		write_report
		write_result '{"version":2,"status":"ready"}'
		;;
	missing-result)
		write_report
		;;
	missing-report)
		write_result "$ready"
		;;
	esac
}

if [ "${FAKE_COPILOT_EARLY:-}" = 1 ]; then
	artifacts
fi

if [ "${FAKE_COPILOT_DELAY:-0}" != 0 ]; then
	sleep "$FAKE_COPILOT_DELAY"
fi

if [ -n "$ctl" ] && [ -f "$ctl/hold" ]; then
	while [ ! -f "$ctl/release.$item_id" ] && [ ! -f "$ctl/release.all" ]; do
		sleep 0.05
	done
fi

if [ "$mode" = hang ]; then
	while :; do sleep 1; done
fi

if [ "${FAKE_COPILOT_EARLY:-}" != 1 ]; then
	artifacts
fi

if [ -n "$ctl" ]; then
	echo "end $item_id $run_n" >>"$ctl/log"
fi
echo '{"type":"result","timestamp":"2026-01-01T00:00:00Z","data":{}}'

if [ "$mode" = nonzero ]; then
	exit 3
fi
exit 0
