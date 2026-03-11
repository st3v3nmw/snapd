#!/bin/bash

#shellcheck source=tests/lib/pkgdb.sh
. "$TESTSLIB"/pkgdb.sh

setup_portals() {
    # Install xdg-desktop-portal and configure service activation for
    # fake portal UI.
    distro_install_package xdg-desktop-portal
    cat << EOF > /usr/share/dbus-1/services/org.freedesktop.impl.portal.spread.service
[D-BUS Service]
Name=org.freedesktop.impl.portal.spread
Exec=/usr/bin/python3 $TESTSLIB/fakeportalui/portalui.py
SystemdService=spread-portal-ui.service
EOF
    cat << EOF > /usr/lib/systemd/user/spread-portal-ui.service
[Unit]
Description=Fake portal UI
[Service]
Type=dbus
BusName=org.freedesktop.impl.portal.spread
ExecStart=/usr/bin/python3 $TESTSLIB/fakeportalui/portalui.py
EOF
    mkdir -p /usr/share/xdg-desktop-portal/portals
    # Disable any existing portal implementations
    for p in /usr/share/xdg-desktop-portal/portals/*.portal; do
        if [ ! -f "$p" ]; then
            continue
        fi
        mv "$p" "$p.disabled"
    done
    cat << EOF > /usr/share/xdg-desktop-portal/portals/spread.portal
[portal]
DBusName=org.freedesktop.impl.portal.spread
Interfaces=org.freedesktop.impl.portal.FileChooser;org.freedesktop.impl.portal.Screenshot;org.freedesktop.impl.portal.AppChooser;
UseIn=spread
EOF

    tests.session -u test exec systemctl --user set-environment XDG_CURRENT_DESKTOP=spread

    # Portals require a graphical session. On Ubuntu 26.04 and newer,
    # xdg-desktop-portal.service has a hard dependency on graphical-session.target
    # (via Requisite=), so it will not start unless a graphical session is active.
    # The test environment does not provide a graphical session, so we start a
    # fake one to allow xdg-desktop-portal to run.
    # In earlier Ubuntu releases the unit only used After=graphical-session.target,
    # which allowed the service to start even when no graphical session was
    # present, so the tests worked in the default environment.
    if os.query is-ubuntu-ge 26.04; then
        sed -i '/^Requisite=graphical-session.target/d' /usr/lib/systemd/user/xdg-desktop-portal.service
        systemctl daemon-reload
    fi
}

teardown_portals() {
    rm -f /usr/share/dbus-1/services/org.freedesktop.impl.portal.spread.service
    rm -f /usr/lib/systemd/user/spread-portal-ui.service
    rm -f /usr/share/xdg-desktop-portal/portals/spread.portal
    # Re-enable any disabled portal implementations
    for p in /usr/share/xdg-desktop-portal/portals/*.portal.disabled; do
        if [ ! -f "$p" ]; then
            continue
        fi
        mv "$p" "/usr/share/xdg-desktop-portal/portals/$(basename "$p" .disabled)"
    done

    distro_purge_package xdg-desktop-portal
    distro_auto_remove_packages
}
