#!/bin/sh
# vds-dockerfile.sh BASE_IMAGE: a Dockerfile (on stdout) for a test "VDS"
# from a stock distribution image: systemd as PID 1, sshd with root and
# password logins, and a sudoer "deploy" whose sudo needs its password -
# what provision.TestInstallOnVDS drives node-install.sh on.
# Passwords: root "rootpass", deploy "deploypass".
set -eu
base=$1
# Hashes made here: chpasswd on RHEL-likes goes through PAM, which a stock
# image leaves unable to check the password afterwards.
root_hash=$(openssl passwd -6 rootpass)
deploy_hash=$(openssl passwd -6 deploypass)
cat <<DOCKERFILE
FROM $base
ENV container=docker
RUN set -eu; \\
    if command -v apt-get >/dev/null; then \\
        export DEBIAN_FRONTEND=noninteractive; apt-get update; \\
        apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server sudo curl ca-certificates iproute2 procps passwd; \\
        admin=sudo; \\
    elif command -v dnf >/dev/null; then \\
        dnf install -y systemd openssh-server sudo iproute procps-ng passwd shadow-utils findutils which authselect; \\
        command -v curl >/dev/null || dnf install -y curl; \\
        authselect select minimal --force >/dev/null 2>&1 || authselect select local --force; \\
        admin=wheel; \\
    elif command -v yum >/dev/null; then \\
        yum install -y systemd openssh-server sudo curl iproute procps-ng passwd shadow-utils which; \\
        admin=wheel; \\
    elif command -v pacman >/dev/null; then \\
        pacman -Syu --noconfirm --needed systemd openssh sudo curl iproute2 procps-ng shadow; \\
        admin=wheel; \\
    elif command -v zypper >/dev/null; then \\
        zypper --non-interactive install systemd openssh sudo curl iproute2 procps shadow; \\
        admin=wheel; groupadd -f wheel; \\
        sed -i -e 's/^Defaults targetpw/# &/' -e 's/^ALL[[:space:]]*ALL=(ALL) ALL/# &/' /etc/sudoers; \\
    else echo "unknown package manager" >&2; exit 1; fi; \\
    echo "%\$admin ALL=(ALL) ALL" > /etc/sudoers.d/90-test; chmod 0440 /etc/sudoers.d/90-test; \\
    usermod -p '$root_hash' root; \\
    useradd -m -s /bin/sh deploy; usermod -aG "\$admin" deploy; usermod -p '$deploy_hash' deploy; \\
    # RHEL-likes ship /etc/shadow 0000, readable only with CAP_DAC_OVERRIDE,
    # which sshd lacks in the container: pam_unix could not check a password.
    chmod 0600 /etc/shadow /etc/gshadow 2>/dev/null || true; \\
    ssh-keygen -A; \\
    sed -i '1i PermitRootLogin yes\\nPasswordAuthentication yes\\nUsePAM yes' /etc/ssh/sshd_config; \\
    rm -f /etc/ssh/sshd_config.d/*.conf 2>/dev/null || true; \\
    rm -f /run/nologin /etc/nologin; \\
    systemctl enable sshd 2>/dev/null || systemctl enable ssh; \\
    systemctl mask getty.target console-getty.service systemd-firstboot.service 2>/dev/null || true; \\
    ln -sf "\$(ls /lib/systemd/systemd /usr/lib/systemd/systemd 2>/dev/null | head -n 1)" /usr/local/sbin/test-init
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/local/sbin/test-init"]
DOCKERFILE
