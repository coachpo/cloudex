#!/bin/sh
set -eu
umask 077

mkdir -p /run/sshd /run/cloudex-ssh
chmod 0700 /var/lib/cloudex-ssh
host_key=/var/lib/cloudex-ssh/ssh_host_ed25519_key
if [ ! -f "$host_key" ]; then
    staging=$(mktemp -d /var/lib/cloudex-ssh/key.XXXXXXXX)
    trap 'rm -f "$staging/key" "$staging/key.pub"; rmdir "$staging"' EXIT
    ssh-keygen -q -t ed25519 -N '' -f "$staging/key"
    mv "$staging/key" "$host_key"
    rm "$staging/key.pub"
    rmdir "$staging"
    trap - EXIT
fi
if [ ! -f /run/cloudex-ssh/authorized_keys ]; then
    touch /run/cloudex-ssh/authorized_keys
fi
chmod 0755 /run/cloudex-ssh /run/sshd
chmod 0644 /run/cloudex-ssh/authorized_keys
exec /usr/sbin/sshd -D -e -f /etc/cloudex-sshd_config
