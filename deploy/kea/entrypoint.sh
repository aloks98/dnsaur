#!/bin/sh
# Entrypoint of the kea image variant (Dockerfile.kea): start the engine,
# then run dnsaur.
#
# kea-dhcp4 is started with the bundled minimal configuration and nothing
# else — it only has to be running and answering on its control socket, and
# dnsaur replaces its whole configuration with the rendered one as soon as
# it connects. Mount your own file at /etc/kea/kea-dhcp4.conf to change what
# it starts with; keep the control socket where this file puts it, because
# DNSAUR_KEA_SOCKET is what dnsaur dials and Kea keeps the socket it was
# started with.
set -e

# 750 is Kea's own packaging convention for this directory, and here both
# processes are root, so nothing else needs to reach it.
mkdir -p /run/kea /var/lib/kea
chmod 750 /run/kea

# ponytail: no supervisor. If kea-dhcp4 dies the container keeps running
# with DNS up and DHCP reported unreachable, which is the honest state and
# the one a restart policy on the container cannot express any better. Use
# a process supervisor in the image if you want the engine restarted in
# place.
kea-dhcp4 -c /etc/kea/kea-dhcp4.conf &
kea_pid=$!

# Wait for the control socket, but never for long. dnsaur handles an engine
# that is not there — the DHCP page says so, and the next lease poll renders
# — so the wait is only worth having because it saves the first poll; a
# minute of it would just be a container that looks hung.
waited=0
while [ ! -S /run/kea/kea.sock ] && [ "$waited" -lt 30 ]; do
	waited=$((waited + 1))
	sleep 1
done
if [ ! -S /run/kea/kea.sock ]; then
	echo "kea-dhcp4 has not opened /run/kea/kea.sock after ${waited}s; starting dnsaur anyway" >&2
fi

# dnsaur is run rather than exec'd, so this shell stays PID 1 and can pass
# the stop signal on. Under exec, `docker stop` reached dnsaur alone and
# kea-dhcp4 was killed with the container a moment later — which costs it
# the chance to write out its lease memfile, so leases handed out since the
# last flush come back as free addresses.
dnsaur "$@" &
dnsaur_pid=$!
trap 'kill -TERM "$dnsaur_pid" "$kea_pid" 2>/dev/null' INT TERM

# Two waits, because a POSIX shell returns from wait as soon as a trapped
# signal is handled, with the job still running: the first returns when the
# trap fires, the second when dnsaur has actually finished shutting down.
# set +e for both — an interrupted wait reports 128+signal, and this is not
# the failure `set -e` is for.
set +e
wait "$dnsaur_pid"
status=$?
if [ "$status" -gt 128 ]; then
	wait "$dnsaur_pid"
	status=$?
fi
# The engine outlives dnsaur by exactly as long as it takes to flush.
kill -TERM "$kea_pid" 2>/dev/null
wait "$kea_pid"
exit "$status"
