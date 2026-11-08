#!/bin/sh
# Runs a command (the Vitest browser projects) in a private network namespace
# that holds only a loopback interface.
#
# Why: Chromium watches the host's interfaces over netlink, and when an IP
# address appears or disappears anywhere (a Docker or kind network created or
# removed, a Wi-Fi lease or IPv6 address renewed) it flushes its socket pools
# with ERR_NETWORK_CHANGED. That aborts every in-flight request, localhost
# included, so the test file a worker was importing fails with "Failed to
# fetch dynamically imported module". In a namespace of its own the browser
# sees no such change; the Vitest server and Chromium both live in it and talk
# over its loopback, and the tests need no other network.
#
# The namespace needs unprivileged user namespaces and `ip`. Where those are
# missing (some containers), the command runs as is and a notice says so.
# PTB_VITEST_NETNS=0 skips the namespace; PTB_VITEST_NETNS=1 makes it required.
set -eu

mode=${PTB_VITEST_NETNS:-auto}
if [ "$mode" != 0 ]; then
	if command -v unshare >/dev/null && command -v ip >/dev/null &&
		unshare --user --map-root-user --net ip link set lo up 2>/dev/null; then
		exec unshare --user --map-root-user --net sh -c 'ip link set lo up && exec "$@"' sh "$@"
	fi
	if [ "$mode" = 1 ]; then
		echo "netns.sh: PTB_VITEST_NETNS=1 but no private network namespace can be created (unshare/ip, user namespaces)" >&2
		exit 2
	fi
	echo "netns.sh: no private network namespace here; a host network change during the run can abort the browser's requests (ERR_NETWORK_CHANGED)" >&2
fi
exec "$@"
