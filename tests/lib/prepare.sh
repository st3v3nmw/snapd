#!/bin/bash

set -eux

# shellcheck source=tests/lib/snaps.sh
. "$TESTSLIB/snaps.sh"
# shellcheck source=tests/lib/pkgdb.sh
. "$TESTSLIB/pkgdb.sh"
# shellcheck source=tests/lib/state.sh
. "$TESTSLIB/state.sh"
#shellcheck source=tests/lib/core-initrd.sh
. "$TESTSLIB"/core-initrd.sh

disable_kernel_rate_limiting() {
    # kernel rate limiting hinders debugging security policy so turn it off
    echo "Turning off kernel rate-limiting"
    # TODO: we should be able to run the tests with rate limiting disabled so
    # debug output is robust, but we currently can't :(
    echo "SKIPPED: see https://forum.snapcraft.io/t/snapd-spread-tests-should-be-able-to-run-with-kernel-rate-limiting-disabled/424"
    #sysctl -w kernel.printk_ratelimit=0
}

disable_journald_rate_limiting() {
    # Disable journald rate limiting
    mkdir -p /etc/systemd/journald.conf.d
    # The RateLimitIntervalSec key is not supported on some systemd versions causing
    # the journal rate limit could be considered as not valid and discarded in consequence.
    # RateLimitInterval key is supported in old systemd versions and in new ones as well,
    # maintaining backward compatibility.
    cat <<-EOF > /etc/systemd/journald.conf.d/no-rate-limit.conf
    [Journal]
    RateLimitInterval=0
    RateLimitBurst=0
EOF
    systemctl restart systemd-journald.service
}

disable_journald_start_limiting() {
    # Disable journald start limiting
    mkdir -p /etc/systemd/system/systemd-journald.service.d
    cat <<-EOF > /etc/systemd/system/systemd-journald.service.d/no-start-limit.conf
    [Unit]
    StartLimitBurst=0
EOF
    systemctl daemon-reload
}

# Set of helpers for checking if the test system is expected to be
# Ubuntu Core. The helpers can be used both in a UC system or in
# a classic system which will be transformed into UC. Note, the 
# helpers assume a specific formatting of SPREAD_SYSTEM environment
# variable which follows this pattern: ubuntu-core-<VERSION>[-ARCH]-<BITS>
# where arch is "" for amd64, arm for armhf and arm64, etc
is_test_target_core() {
    local VERSION=${1:-}
    [[ "$SPREAD_SYSTEM" = ubuntu-core-${VERSION}* ]]
}

is_test_target_core_ge() {
    local VERSION=${1:-}
    if [ -z "$VERSION" ]; then
        echo "version id is expected"
        exit 1
    fi
    CURR_VERSION="$(cut -d- -f3 <<< "$SPREAD_SYSTEM")"
    [ "$CURR_VERSION" -ge "${VERSION}" ]
}

is_test_target_core_le() {
    local VERSION=${1:-}
    if [ -z "$VERSION" ]; then
        echo "version id is expected"
        exit 1
    fi
    CURR_VERSION="$(cut -d- -f3 <<< "$SPREAD_SYSTEM")"
    [ "$CURR_VERSION" -le "${VERSION}" ]
}

disable_refreshes() {
    echo "Modify state to make it look like the last refresh just happened"
    systemctl stop snapd.socket snapd.service
    "$TESTSTOOLS"/snapd-state prevent-autorefresh
    systemctl start snapd.socket snapd.service

    echo "Minimize risk of hitting refresh schedule"
    snap set core refresh.schedule=00:00-23:59
    snap refresh --time --abs-time | MATCH "last: 2[0-9]{3}"
}

setup_snapd_proxy() {
    if [ "${SNAPD_USE_PROXY:-}" != true ]; then
        return
    fi

    mkdir -p /etc/systemd/system/snapd.service.d
    cat <<EOF > /etc/systemd/system/snapd.service.d/proxy.conf
[Service]
Environment="HTTPS_PROXY=$HTTPS_PROXY HTTP_PROXY=$HTTP_PROXY https_proxy=$HTTPS_PROXY http_proxy=$HTTP_PROXY NO_PROXY=$NO_PROXY no_proxy=$NO_PROXY"
EOF

    # We change the service configuration so reload and restart
    # the units to get them applied
    systemctl daemon-reload
    # restart the service (it pulls up the socket)
    systemctl restart snapd.service
}

setup_system_proxy() {
    mkdir -p "$SNAPD_WORK_DIR"
    if [ "${SNAPD_USE_PROXY:-}" = true ]; then    
        cp -f /etc/environment "$SNAPD_WORK_DIR"/environment.bak
        {
            echo "HTTPS_PROXY=$HTTPS_PROXY"
            echo "HTTP_PROXY=$HTTP_PROXY"
            echo "https_proxy=$HTTPS_PROXY"
            echo "http_proxy=$HTTP_PROXY"
            echo "NO_PROXY=$NO_PROXY"
            echo "no_proxy=$NO_PROXY"
        } >> /etc/environment
    fi
}

setup_systemd_snapd_overrides() {
    local burst
    burst=10
    if [ "$SPREAD_BACKEND" = "garden" ]; then
        # the tests execute much faster and the repeated stop/start of snapd in
        # prepare may eventually go over the start limit
        burst=30
    fi
    mkdir -p /etc/systemd/system/snapd.service.d
    cat <<EOF > /etc/systemd/system/snapd.service.d/local.conf
[Service]
Environment=SNAPD_DEBUG_HTTP=7 SNAPD_DEBUG=1 SNAPPY_TESTING=1 SNAPD_REBOOT_DELAY=10m SNAPD_CONFIGURE_HOOK_TIMEOUT=30s SNAPPY_USE_STAGING_STORE=$SNAPPY_USE_STAGING_STORE
ExecStartPre=/bin/touch /dev/iio:device0

[Unit]
# The default limit is usually 5, which can be easily hit in 
# a fast system with few systemd units
StartLimitBurst=${burst}
StartLimitIntervalSec=10s
EOF

    mkdir -p /etc/systemd/system/snapd.socket.d
    cat <<EOF > /etc/systemd/system/snapd.socket.d/local.conf
[Unit]
# The default limit is usually 5, which can be easily hit in
# a fast system with few systemd units
StartLimitBurst=${burst}
StartLimitIntervalSec=10s
EOF

    # We change the service configuration so reload and restart
    # the units to get them applied
    systemctl daemon-reload
    # stop the socket (it pulls down the service)
    systemctl stop snapd.socket
    # start the service (it pulls up the socket)
    systemctl start snapd.service
}

# setup_experimental_features enables experimental snapd features passed
# via optional EXPERIMENTAL_FEATURES environment variable. The features must be
# separated by commas and "experimental." prefixes should be omitted.
setup_experimental_features() {
    if [ -n "$EXPERIMENTAL_FEATURES" ]; then
        echo "$EXPERIMENTAL_FEATURES" | while IFS="," read -r FEATURE; do
            echo "Enabling feature experimental.$FEATURE"
            snap set system "experimental.$FEATURE"=true
        done
    else
        echo "There are no experimental snapd features to enable"
    fi
}

save_installed_core_snap() {
    local target_dir="${1-}"

    SNAP_MOUNT_DIR="$(os.paths snap-mount-dir)"
    core="$(readlink -f "$SNAP_MOUNT_DIR"/core/current)"
    snap="$(mount | awk -v core="$core" '{ if ($3 == core) print $1 }' | head -n1)"
    snap_name="$(basename "$snap")"

    # make a copy for later use
    if [ -n "$target_dir" ]; then
        mkdir -p "$target_dir"

        cp -av "$snap" "${target_dir}/${snap_name}"
        cp "$snap" "${target_dir}/${snap_name}.orig"
    fi
}


# update_core_snap_for_classic_reexec modifies the core snap for snapd re-exec
# by injecting binaries from the installed snapd deb built from our modified code.
# $1: directory where updated core snap should be copied (optional)
update_core_snap_for_classic_reexec() {
    local target_dir="${1-}"

    # it is possible to disable this to test that snapd (the deb) works
    # fine with whatever is in the core snap
    if [ "$MODIFY_CORE_SNAP_FOR_REEXEC" != "1" ]; then
        echo "Not modifying the core snap as requested via MODIFY_CORE_SNAP_FOR_REEXEC"
        return
    fi

    # We want to use the in-tree snap/snapd/snap-exec/snapctl, because
    # we re-exec by default.
    # To accomplish that, we'll just unpack the core we just grabbed,
    # shove the new snap-exec and snapctl in there, and repack it.
    SNAP_MOUNT_DIR="$(os.paths snap-mount-dir)"
    LIBEXEC_DIR="$(os.paths libexec-dir)"

    # First of all, unmount the core
    core="$(readlink -f "$SNAP_MOUNT_DIR"/core/current)"
    snap="$(mount | awk -v core="$core" '{ if ($3 == core) print $1 }' | head -n1)"
    umount --verbose "$core"

    # Now unpack the core, inject the new snap-exec/snapctl into it
    unsquashfs -no-progress "$snap"
    # clean the old snapd binaries, just in case
    rm squashfs-root/usr/lib/snapd/* squashfs-root/usr/bin/snap
    # and copy in the current libexec
    cp -a "$LIBEXEC_DIR"/snapd/* squashfs-root/usr/lib/snapd/
    # also the binaries themselves
    cp -a /usr/bin/snap squashfs-root/usr/bin/
    # make sure bin/snapctl is a symlink to lib/
    if [ ! -L squashfs-root/usr/bin/snapctl ]; then
        rm -f squashfs-root/usr/bin/snapctl
        ln -s ../lib/snapd/snapctl squashfs-root/usr/bin/snapctl
    fi

    case "$SPREAD_SYSTEM" in
        ubuntu-*|debian-*)
            # and snap-confine's apparmor
            if [ -e /etc/apparmor.d/usr.lib.snapd.snap-confine.real ]; then
                cp -a /etc/apparmor.d/usr.lib.snapd.snap-confine.real squashfs-root/etc/apparmor.d/usr.lib.snapd.snap-confine.real
            else
                cp -a /etc/apparmor.d/usr.lib.snapd.snap-confine      squashfs-root/etc/apparmor.d/usr.lib.snapd.snap-confine.real
            fi
            ;;
    esac

    case "$SPREAD_SYSTEM" in
        ubuntu-*)
            # also load snap-confine's apparmor profile
            apparmor_parser -r squashfs-root/etc/apparmor.d/usr.lib.snapd.snap-confine.real
            ;;
    esac

    case "$SPREAD_SYSTEM" in
        fedora-*|centos-*|amazon-*)
            if selinuxenabled ; then
                # On these systems just unpacking core snap to $HOME will
                # automatically apply user_home_t label on all the contents of the
                # snap; since we cannot drop xattrs when calling mksquashfs, make
                # sure that we relabel the contents in way that a squashfs image
                # without any labels would look like: system_u:object_r:unlabeled_t
                chcon -R -u system_u -r object_r -t unlabeled_t squashfs-root
            fi
            ;;
    esac

    # Debian packages don't carry permissions correctly and we use post-inst
    # hooks to fix that on classic systems. Here, as a special case, fix the
    # void directory.
    chmod 111 squashfs-root/var/lib/snapd/void

    # repack, cheating to speed things up (4sec vs 1.5min)
    mv "$snap" "${snap}.orig"
    mksnap_fast "squashfs-root" "$snap"
    chmod --reference="${snap}.orig" "$snap"
    rm -rf squashfs-root

    # make a copy for later use
    if [ -n "$target_dir" ]; then
        mkdir -p "$target_dir"
        cp -av "$snap" "$target_dir/"
    fi

    # Now mount the new core snap, first discarding the old mount namespace
    snapd.tool exec snap-discard-ns core
    mount "$snap" "$core"

    check_file() {
        if ! cmp "$1" "$2" ; then
            echo "$1 in tree and $2 in core snap are unexpectedly not the same"
            exit 1
        fi
    }

    # Make sure we're running with the correct copied bits
    for p in "$LIBEXEC_DIR/snapd/snap-exec" "$LIBEXEC_DIR/snapd/snap-confine" "$LIBEXEC_DIR/snapd/snap-discard-ns" "$LIBEXEC_DIR/snapd/snapd" "$LIBEXEC_DIR/snapd/snap-update-ns"; do
        check_file "$p" "$core/usr/lib/snapd/$(basename "$p")"
    done
    for p in /usr/bin/snapctl /usr/bin/snap; do
        check_file "$p" "$core$p"
    done
}

prepare_memory_limit_override() {
    # set up memory limits for snapd bu default unless explicit requested not to
    # or the system is known to be problematic
    local set_limit=1

    case "$SPREAD_SYSTEM" in
        ubuntu-core-16-*|ubuntu-core-18-*|ubuntu-16.04-*|ubuntu-18.04-*)
            # the tests on UC16, UC18 and correspondingly 16.04 and 18.04 have
            # demonstrated that the memory limit state claimed by systemd may be
            # out of sync with actual memory controller setting for the
            # snapd.service cgroup
            set_limit=0
            ;;
        amazon-linux-*)
            # similar issues have been observed on Amazon Linux 2
            set_limit=0
            ;;
        *)
            if [ "$SNAPD_NO_MEMORY_LIMIT" = 1 ]; then
                set_limit=0
            fi
            ;;
    esac

    # If we don't wish to impose a memory limit, and the conf file 
    # already doesn't exist, then no new configuration is needed
    if [ "$set_limit" == "0" ] && ! [ -f "/etc/systemd/system/snapd.service.d/memory-max.conf" ]; then
        return
    fi

    # If we wish to impose a memory limit, and the conf file 
    # already exists, then no new configuration is needed
    if [ "$set_limit" == "1" ] && [ -f "/etc/systemd/system/snapd.service.d/memory-max.conf" ]; then
        return
    fi

    if [ "$set_limit" = "0" ]; then
        # make sure the file does not exist then
        rm -f /etc/systemd/system/snapd.service.d/memory-max.conf
    else
        mkdir -p /etc/systemd/system/snapd.service.d
        # Use MemoryMax to set the memory limit for snapd.service, that is the
        # main snapd process and its subprocesses executing within the same
        # cgroup. If snapd hits the memory limit, it will get killed by
        # oom-killer which will be caught in restore_project_each in
        # prepare-restore.sh.
        #
        # This ought to set MemoryMax, but on systems with older systemd we need to
        # use MemoryLimit, which is deprecated and replaced by MemoryMax now, but
        # systemd is backwards compatible so the limit is still set.
        cat <<EOF > /etc/systemd/system/snapd.service.d/memory-max.conf
[Service]
MemoryLimit=200M
EOF
    fi
    # the service setting may have changed in the service so we need
    # to ensure snapd is reloaded
    systemctl daemon-reload
    systemctl restart snapd
}

prepare_reexec_override() {
    local reexec_file=/etc/systemd/system/snapd.service.d/reexec.conf
 
    # First time it is needed to save the initial env var value
    if not tests.env is-set initial SNAP_REEXEC; then
        tests.env set initial SNAP_REEXEC "$SNAP_REEXEC"
    # Then if the new value is the same than the initial, then no new configuration needed
    elif [ "$(tests.env get initial SNAP_REEXEC)" = "$SNAP_REEXEC" ]; then
        return
    fi

    # Just update reexec configuration when the SNAP_REEXEC var has been updated
    # Otherwise it is used the configuration set during project preparation
    mkdir -p /etc/systemd/system/snapd.service.d
    if [ -z "${SNAP_REEXEC:-}" ]; then
        rm -f "$reexec_file"
    else
        cat <<EOF > "$reexec_file"
[Service]
Environment=SNAP_REEXEC=$SNAP_REEXEC
EOF
    fi

    systemctl daemon-reload
    systemctl restart snapd
}

prepare_each_classic() {
    if [ ! -f /etc/systemd/system/snapd.service.d/local.conf ]; then
        echo "/etc/systemd/system/snapd.service.d/local.conf vanished!"
        exit 1
    fi

    prepare_reexec_override
    # Each individual task may potentially set the SNAP_NO_MEMORY_LIMIT variable
    prepare_memory_limit_override
}

prepare_each_core() {
    # Each individual task may potentially set the SNAP_NO_MEMORY_LIMIT variable
    prepare_memory_limit_override
}

prepare_classic() {
    # Configure the proxy in the system when it is required
    setup_system_proxy   

    # Skip building snapd when REUSE_SNAPD is set to 1
    if [ "$REUSE_SNAPD" != 1 ]; then
        distro_install_build_snapd
    fi

    if snap --version |MATCH unknown; then
        echo "Package build incorrect, 'snap --version' mentions 'unknown'"
        snap --version
        distro_query_package_info snapd
        exit 1
    fi
    if snapd.tool exec snap-confine --version | MATCH unknown; then
        echo "Package build incorrect, 'snap-confine --version' mentions 'unknown'"
        snapd.tool exec snap-confine --version
        case "$SPREAD_SYSTEM" in
            ubuntu-*|debian-*)
                apt-cache policy snapd
                ;;
            fedora-*)
                dnf info snapd
                ;;
        esac
        exit 1
    fi

    # Some systems (google:ubuntu-16.04-64) ship with a broken sshguard
    # unit. Stop the broken unit to not confuse the "degraded-boot" test.
    #
    # Some other (debian-sid) fail in fwupd-refresh.service
    #
    # FIXME: fix the ubuntu-16.04-64 image
    # FIXME2: fix the debian-sid-64 image
    for svc in fwupd-refresh.service sshguard.service; do
        if systemctl list-unit-files | grep "$svc"; then
            if systemctl is-failed "$svc"; then
                systemctl stop "$svc"
	        systemctl reset-failed "$svc"
            fi
        fi
    done

    # Install snapd snap to ensure re-exec to snapd snap instead of snapd in core.
    # This also prevents snapd from automatically installing snapd snap as
    # prerequisite for installing any non-base snap introduced in PR#14173.
    if snap list snapd ; then
        snap info snapd
        echo "Error: not expecting snapd snap to be installed"
        exit 1
    else
        build_dir="$SNAPD_WORK_DIR/snapd_snap_for_classic"
        rm -rf "$build_dir"
        mkdir -p "$build_dir"
        build_snapd_snap "$build_dir"
        snap install --dangerous "$build_dir/"snapd_*.snap
        snap wait system seed.loaded
    fi
    snap list snapd

    setup_snapd_proxy

    mount_dir="$(os.paths snap-mount-dir)"
    if ! getcap "$mount_dir"/snapd/current/usr/lib/snapd/snap-confine | grep "cap_sys_admin"; then
        echo "snapd snap is missing file capabilities on snap-confine"
        echo "and is not usable"
        echo "ensure it has been correctly built (wipe snapcraft containers and rebuild)"
        exit 1
    fi

    setup_systemd_snapd_overrides

    if [ "$REMOTE_STORE" = staging ]; then
        # reset seeding data that is likely tainted with production keys
        systemctl stop snapd.service snapd.socket
        rm -rf /var/lib/snapd/assertions/*
        rm -f /var/lib/snapd/state.json
        "$TESTSTOOLS"/store-state setup-staging-store
    fi

    # Snapshot the state including core.
    if ! is_snapd_state_saved; then
        # Create the file with the initial environment before saving the state
        tests.env start initial

        # need to be seeded to proceed with snap install
        # also make sure the captured state is seeded
        snap wait system seed.loaded

        # Cache snaps
        # shellcheck disable=SC2086
        cache_snaps core core18 ${PRE_CACHE_SNAPS}
        if os.query is-pc-amd64; then
            cache_snaps core20
        fi

        # now use parameterized core channel (defaults to edge) instead
        # of a fixed one and close to stable in order to detect defects
        # earlier
        if snap list core ; then
            snap refresh --"$CORE_CHANNEL" core
        else
            snap install --"$CORE_CHANNEL" core
        fi

        snap list | grep core

        # With reexec, and on classic, the snapd snap is preferred over the core snap for reexecution target,
        # so to be as close as possible to the actual real life scenarios, we only update the snapd snap.
        # The tests alreday ensure that snapd snap is installed.
        if tests.info is-snapd-from-archive; then
            save_installed_core_snap "$TESTSTMP/core_snap"
        else
            systemctl stop snapd.{service,socket}
            # repack and also make a side copy of the core snap
            update_core_snap_for_classic_reexec "$TESTSTMP/core_snap"
            systemctl start snapd.{service,socket}
        fi

        prepare_reexec_override
        prepare_state_lock "SNAPD PROJECT"
        prepare_tag_features
        prepare_memory_limit_override
        disable_refreshes

        # Check bootloader environment output in architectures different to s390x which uses zIPL
        if ! [ "$(uname  -m)" = "s390x" ]; then
            echo "Ensure that the bootloader environment output does not contain any of the snap_* variables on classic"
            # shellcheck disable=SC2119
            output=$("$TESTSTOOLS"/boot-state bootenv show)
            if echo "$output" | MATCH snap_ ; then
                echo "Expected bootloader environment without snap_*, got:"
                echo "$output"
                exit 1
            fi
        fi

        # lxd-installer is in cloud images starting from 24.04. This package
        # installs lxd when any lxc command is run. This caused problems
        # because if we install snapcraft & lxd, in the restore step lxd is
        # removed, and after that snapcraft is removed. However, snapcraft's
        # remove hook calls lxd and triggers a new installation of lxd, and in
        # turn when we try to remove core22, it fails as lxd has been
        # re-installed and depends on that base. Therefore, we remove it to
        # prevent these issues, and we do that before we get the list of
        # installed packages to make sure we do not re-install it again.
        if ( os.query is-ubuntu || os.query is-debian ) && tests.pkgs is-installed lxd-installer; then
            extra=
            if os.query is-ubuntu-ge 25.10; then
                # the following dependency is in place in 25.10:
                # ubuntu-server:amd64 Depends lxd-installer
                #
                # NOTE: this will leave some packages without explicit
                # dependency pulling them in
                extra=ubuntu-server
            fi
            apt remove -y --purge lxd-installer $extra
        fi

        setup_experimental_features

        systemctl stop snapd.{service,socket}
        save_snapd_state
        systemctl start snapd.socket
    fi

    disable_kernel_rate_limiting

    if os.query is-arch-linux; then
        # Arch packages do not ship empty directories by default, hence there is
        # no /etc/dbus-1/system.d what prevents dbus from properly establishing
        # inotify watch on that path
        mkdir -p /etc/dbus-1/system.d
        systemctl reload dbus.service
    fi
}

ensure_snapcraft() {
    if ! command -v snapcraft; then
        snap install --channel="${SNAPCRAFT_SNAP_CHANNEL}" snapcraft --classic
        "$TESTSTOOLS"/lxd-state prepare-snap
    fi
}

cleanup_snapcraft() {
    snap remove --purge lxd || true
    "$TESTSTOOLS"/lxd-state undo-mount-changes
    snap remove --purge snapcraft || true
    # TODO there should be some smarter cleanup helper which removes all snaps
    # in the right order
    # base snap of both lxd and snapcraft
    snap remove --purge core22 || true
}

run_snapcraft() {
    ensure_snapcraft
    # maybe read the log path from snapcraft output as an improvement
    # but this works
    if ! (cd "${PROJECT_PATH}" && snapcraft "$@"); then
        # shellcheck disable=SC2012
        tail -n1000 "${HOME}/.local/state/snapcraft/log/$(ls -t "${HOME}/.local/state/snapcraft/log/" -1 | head -n1)"
        false
    fi
    cleanup_snapcraft
}

build_snapd_snap() {
    local TARGET
    local snapd_snap_cache
    TARGET="${1}"

    snapd_snap_cache="$SNAPD_WORK_DIR/snapd_snap"
    mkdir -p "${snapd_snap_cache}"
    for snap in "${snapd_snap_cache}"/snapd_*.snap; do
        if ! [ -f "${snap}" ]; then
            if [ "${USE_PREBUILT_SNAPD_SNAP}" = true ]; then
                if [ -n "${USE_SNAPD_SNAP_URL}" ]; then
                    wget -q "$USE_SNAPD_SNAP_URL" -O "${snapd_snap_cache}/snapd_from_ci.snap"
                else
                    cp "${PROJECT_PATH}/built-snap"/snapd_1337.*.snap.keep "${snapd_snap_cache}/snapd_from_ci.snap"
                fi
            else
                # This is not reliable across classic releases so only allow on
                # ARM variants as a special case since we cannot cross build
                # snapd snap for ARM right now
                case "$SPREAD_SYSTEM" in
                    *-arm-*)
                        ;;
                    *)
                        echo "ERROR: system $SPREAD_SYSTEM should use a prebuilt snapd snap"
                        echo "see HACKING.md and use tests/build-test-snapd-snap to build one locally"
                        exit 1
                        ;;
                esac
                [ -d "${TARGET}" ] || mkdir -p "${TARGET}"
                touch "${PROJECT_PATH}"/test-build
                chmod -R go+r "${PROJECT_PATH}/tests"
                # TODO: run_snapcraft does not currently guarantee or check the required version for building snapd
                run_snapcraft --use-lxd --verbosity quiet --output="snapd_from_snapcraft.snap"
                mv "${PROJECT_PATH}"/snapd_from_snapcraft.snap "${snapd_snap_cache}"
            fi
        fi
        break
    done

    cp "${snapd_snap_cache}"/snapd_*.snap "${TARGET}/"
}

build_snapd_snap_with_run_mode_firstboot_tweaks() {
    local snapd_snap_cache
    local TARGET

    TARGET="${1}"

    snapd_snap_cache="$SNAPD_WORK_DIR/snapd_snap_with_tweaks"
    mkdir -p "${snapd_snap_cache}"
    for snap in "${snapd_snap_cache}"/snapd_*.snap; do
        if [ -f "${snap}" ]; then
            cp "${snap}" "${TARGET}/"
            return
        fi
    done

    if [ "${USE_PREBUILT_SNAPD_SNAP}" = true ]; then
        if [ -n "${USE_SNAPD_SNAP_URL}" ]; then
            wget -q "$USE_SNAPD_SNAP_URL" -O /tmp/snapd_from_snapcraft.snap
        else
            cp "${PROJECT_PATH}/built-snap"/snapd_1337.*.snap.keep "/tmp/snapd_from_snapcraft.snap"
        fi
    else
        chmod -R go+r "${PROJECT_PATH}/tests"
        run_snapcraft --use-lxd --verbosity quiet --output="snapd_from_snapcraft.snap"
        mv "${PROJECT_PATH}/snapd_from_snapcraft.snap" "/tmp/snapd_from_snapcraft.snap"
    fi

    # TODO set up a trap to clean this up properly?
    local UNPACK_DIR
    UNPACK_DIR="$(mktemp -d /tmp/snapd-unpack.XXXXXXXX)"
    unsquashfs -no-progress -f -d "$UNPACK_DIR" /tmp/snapd_from_snapcraft.snap

    # now install a unit that sets up enough so that we can connect
    cat > "$UNPACK_DIR"/lib/systemd/system/snapd.spread-tests-run-mode-tweaks.service <<'EOF'
[Unit]
Description=Tweaks to run mode for spread tests
Before=snapd.service
Documentation=man:snap(1)

[Service]
Type=oneshot
ExecStart=/usr/lib/snapd/snapd.spread-tests-run-mode-tweaks.sh
RemainAfterExit=true

[Install]
WantedBy=multi-user.target
EOF
    # XXX: this duplicates a lot of setup_test_user_by_modify_writable()
    cat > "$UNPACK_DIR"/usr/lib/snapd/snapd.spread-tests-run-mode-tweaks.sh <<'EOF'
#!/bin/sh
set -ex
# ensure we don't enable ssh in install mode or spread will get confused
if ! grep -E 'snapd_recovery_mode=(run|recover)' /proc/cmdline; then
    echo "not in run or recovery mode - script not running"
    exit 0
fi
if [ -e /root/spread-setup-done ]; then
    exit 0
fi

# extract data from previous stage
(cd / && tar xf /run/mnt/ubuntu-seed/run-mode-overlay-data.tar.gz)

# user db - it's complicated
for f in group gshadow passwd shadow; do
    # now bind mount read-only those passwd files on boot
    cat >/etc/systemd/system/etc-"$f".mount <<EOF2
[Unit]
Description=Mount root/test-etc/$f over system etc/$f
Before=ssh.service

[Mount]
What=/root/test-etc/$f
Where=/etc/$f
Type=none
Options=bind,ro

[Install]
WantedBy=multi-user.target
EOF2
    systemctl enable etc-"$f".mount
    systemctl start etc-"$f".mount
done

mkdir -p /home/test
chown 12345:12345 /home/test
mkdir -p /home/ubuntu
chown 1000:1000 /home/ubuntu
mkdir -p /etc/sudoers.d/
echo 'test ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers.d/99-test-user
echo 'ubuntu ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers.d/99-ubuntu-user
sed -i 's/\#\?\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config
echo "MaxAuthTries 120" >> /etc/ssh/sshd_config
grep '^PermitRootLogin yes' /etc/ssh/sshd_config
if systemctl is-active ssh; then
   systemctl reload ssh
fi

touch /root/spread-setup-done
EOF
    chmod 0755 "$UNPACK_DIR"/usr/lib/snapd/snapd.spread-tests-run-mode-tweaks.sh

    mkdir -p "${snapd_snap_cache}"
    snap pack "$UNPACK_DIR" "${snapd_snap_cache}/"
    rm -rf "$UNPACK_DIR"
    cp "${snapd_snap_cache}"/snapd_*.snap "${TARGET}/"
}

repack_core_snap_with_tweaks() {
    local CORESNAP="$1"
    local TARGET="$2"

    local UNPACK_DIR
    # TODO set up a trap to clean this up properly?
    UNPACK_DIR="$(mktemp -d /tmp/core-unpack.XXXXXXXX)"
    unsquashfs -no-progress -f -d "$UNPACK_DIR" "$CORESNAP"

    mkdir -p "$UNPACK_DIR"/etc/systemd/journald.conf.d
    cat <<EOF > "$UNPACK_DIR"/etc/systemd/journald.conf.d/to-console.conf
[Journal]
ForwardToConsole=yes
TTYPath=/dev/ttyS0
MaxLevelConsole=debug
EOF
    mkdir -p "$UNPACK_DIR"/etc/systemd/system/snapd.service.d
cat <<EOF > "$UNPACK_DIR"/etc/systemd/system/snapd.service.d/logging.conf
[Service]
Environment=SNAPD_DEBUG_HTTP=7 SNAPD_DEBUG=1 SNAPPY_TESTING=1 SNAPD_CONFIGURE_HOOK_TIMEOUT=30s
StandardOutput=journal+console
StandardError=journal+console
EOF

    if [ "${NESTED_REPACK_FOR_FAKESTORE-}" = "true" ]; then
        cat <<EOF > "$UNPACK_DIR"/etc/systemd/system/snapd.service.d/store.conf
[Service]
Environment=SNAPPY_FORCE_API_URL=http://10.0.2.2:11028
EOF
    fi

    cp "${SPREAD_PATH}"/data/completion/bash/complete.sh "${UNPACK_DIR}"/usr/lib/snapd/complete.sh

    snap pack --filename="$TARGET" "$UNPACK_DIR"

    rm -rf "$UNPACK_DIR"
}

repack_kernel_snap() {
    local TARGET=$1
    local VERSION
    local UNPACK_DIR
    local CHANNEL

    VERSION=$(nested_get_version)
    if [ "$VERSION" = 16 ]; then
        CHANNEL=latest
    else
        CHANNEL=$VERSION
    fi

    echo "Repacking kernel snap"
    # TODO set up a trap to clean this up properly?
    UNPACK_DIR="$(mktemp -d /tmp/kernel-unpack.XXXXXXXX)"
    snap download --basename=pc-kernel --channel="$CHANNEL/${KERNEL_CHANNEL}" pc-kernel
    unsquashfs -no-progress -f -d "$UNPACK_DIR" pc-kernel.snap
    snap pack --filename="$TARGET" "$UNPACK_DIR"

    rm -rf pc-kernel.snap "$UNPACK_DIR"
}

# Builds kernel snap with bad kernel.efi, in different ways
# $1: snap we will modify
# $2: target folder for the new snap
# $3: argument, type of corruption we want for kernel.efi
uc20_build_corrupt_kernel_snap() {
    local ORIG_SNAP="$1"
    local TARGET_DIR="$2"
    local optArg=${3:-}

    # kernel snap is huge, unpacking to current dir
    local REPACKED_DIR=repacked-kernel
    local KERNEL_EFI_PATH=$REPACKED_DIR/kernel.efi
    unsquashfs -d "$REPACKED_DIR" "$ORIG_SNAP"

    case "$optArg" in
        --empty)
            printf "" > "$KERNEL_EFI_PATH"
            ;;
        --zeros)
            dd if=/dev/zero of="$KERNEL_EFI_PATH" count=1
            ;;
        --bad-*)
            section=${optArg#--bad-}
            # Get the file offset for the section, put zeros at the beginning of it
            sectOffset=$(objdump -w -h "$KERNEL_EFI_PATH" | grep "$section" |
                             awk '{print $6}')
            dd if=/dev/zero of="$KERNEL_EFI_PATH" \
               bs=1 seek=$((0x$sectOffset)) count=512 conv=notrunc
            ;;
    esac

    # Make snap smaller, we don't need the fw with qemu
    rm -rf "$REPACKED_DIR"/firmware/*
    snap pack "$REPACKED_DIR" "$TARGET_DIR"
    rm -rf "$REPACKED_DIR"
}

uc_write_bootstrap_wrapper() {
    local SKELETON_PATH="$1"
    local INJECT_ERR="${2:-false}"

    cp -a /usr/lib/snapd/snap-bootstrap "$SKELETON_PATH"/usr/lib/snapd/snap-bootstrap.real
    cat <<'EOF' >"$SKELETON_PATH"/usr/lib/snapd/snap-bootstrap
#!/bin/sh
set -eux
if [ "$1" != initramfs-mounts ]; then
    exec /usr/lib/snapd/snap-bootstrap.real "$@"
fi
beforeDate="$(date --utc '+%s')"
/usr/lib/snapd/snap-bootstrap.real "$@"
if [ -d /run/mnt/data/system-data ]; then
    touch /run/mnt/data/system-data/the-tool-ran
fi
# also copy the time for the clock-epoch to system-data, this is
# used by a specific test but doesn't hurt anything to do this for
# all tests
mode="$(grep -Eo 'snapd_recovery_mode=([a-z]+)' /proc/cmdline)"
mode=${mode##snapd_recovery_mode=}
mkdir -p /run/mnt/ubuntu-seed/test
stat -c '%Y' /usr/lib/clock-epoch >> /run/mnt/ubuntu-seed/test/${mode}-clock-epoch
echo "$beforeDate" > /run/mnt/ubuntu-seed/test/${mode}-before-snap-bootstrap-date
date --utc '+%s' > /run/mnt/ubuntu-seed/test/${mode}-after-snap-bootstrap-date
EOF
    if [ "$INJECT_ERR" = "true" ]; then
        # add a kernel panic to the end of the-tool execution
        echo "echo 'forcibly panicking'; echo c > /proc/sysrq-trigger" >> "$SKELETON_PATH"/usr/lib/snapd/snap-bootstrap
    fi
    chmod +x "$SKELETON_PATH"/usr/lib/snapd/snap-bootstrap
}

uc20_build_initramfs_kernel_snap() {
    quiet apt install software-properties-common -y
    # carries ubuntu-core-initframfs
    quiet add-apt-repository ppa:snappy-dev/image -y
    # On focal, lvm2 does not reinstall properly after being removed.
    # So we need to clean up in case the VM has been re-used.
    if os.query is-focal; then
        systemctl unmask lvm2-lvmpolld.socket
    fi
    # TODO: install the linux-firmware as the current version of
    # ubuntu-core-initramfs does not depend on it, but nonetheless requires it
    # to build the initrd
    quiet apt install ubuntu-core-initramfs linux-firmware -y

    local ORIG_SNAP="$1"
    local TARGET="$2"

    # TODO proper option support here would be nice but bash is hard and this is
    # easier, and likely we won't need to both inject a panic and set the epoch
    # bump simultaneously
    local injectKernelPanic=false
    local initramfsEpochBumpTime
    initramfsEpochBumpTime=$(date '+%s')
    optArg=${3:-}
    case "$optArg" in
        --inject-kernel-panic-in-initramfs)
            injectKernelPanic=true
            ;;
        --epoch-bump-time=*)
            # this strips the option and just gives us the value
            initramfsEpochBumpTime="${optArg#--epoch-bump-time=}"
            ;;
    esac
    
    # kernel snap is huge, unpacking to current dir
    unsquashfs -d repacked-kernel "$ORIG_SNAP"

    # repack initrd magic, beware
    # assumptions: initrd is compressed with LZ4, cpio block size 512, microcode
    # at the beginning of initrd image
    (
        cd repacked-kernel
        unpackeddir="$PWD"
        #shellcheck disable=SC2010
        kver=$(ls "config"-* | grep -Po 'config-\K.*')

        # XXX: ideally we should unpack the initrd, replace snap-boostrap and
        # repack it using ubuntu-core-initramfs --skeleton=<unpacked> this does not
        # work and the rebuilt kernel.efi panics unable to start init, but we
        # still need the unpacked initrd to get the right kernel modules
        objcopy -j .initrd -O binary kernel.efi initrd
        # this works on 20.04 but not on 18.04
        unmkinitramfs initrd unpacked-initrd

        # use only the initrd we got from the kernel snap to inject our changes
        # we don't use the distro package because the distro package may be 
        # different systemd version, etc. in the initrd from the one in the 
        # kernel and we don't want to test that, just test our snap-bootstrap
        cp -ar unpacked-initrd skeleton
        # all the skeleton edits go to a local copy of distro directory
        skeletondir="$PWD/skeleton"
        initrd_dir="$skeletondir/main"
        clock_epoch_file="$skeletondir/main/usr/lib/clock-epoch"
        if os.query is-arm; then
            initrd_dir="$skeletondir"
            clock_epoch_file="$skeletondir/usr/lib/clock-epoch"
        fi
        uc_write_bootstrap_wrapper "$initrd_dir" "$injectKernelPanic"

        # bump the epoch time file timestamp, converting unix timestamp to 
        # touch's date format
        touch -t "$(date --utc "--date=@$initramfsEpochBumpTime" '+%Y%m%d%H%M')" "$clock_epoch_file"

        # copy any extra files to the same location inside the initrd
        if [ -d ../extra-initrd/ ]; then
            if os.query is-arm; then
                cp -a ../extra-initrd/* "$skeletondir"
            else
                cp -a ../extra-initrd/* "$skeletondir"/main
            fi
        fi

        # XXX: need to be careful to build an initrd using the right kernel
        # modules from the unpacked initrd, rather than the host which may be
        # running a different kernel
        (
            # accommodate assumptions about tree layout, use the unpacked initrd
            # to pick up the right modules
            if os.query is-arm; then
                cd unpacked-initrd
                feature='.'
            else
                cd unpacked-initrd/main
                feature='main'
            fi
            # XXX: pass feature 'main' and u-c-i picks up any directory named
            # after feature inside skeletondir and uses that a template
            ubuntu-core-initramfs create-initrd \
                                  --kernelver "$kver" \
                                  --skeleton "$skeletondir" \
                                  --kerneldir "${unpackeddir}/modules/$kver" \
                                  --firmwaredir "${unpackeddir}/firmware" \
                                  --feature "$feature" \
                                  --output "$unpackeddir"/repacked-initrd
        )

        # copy out the kernel image for create-efi command
        objcopy -j .linux -O binary kernel.efi "vmlinuz-$kver"

        # assumes all files are named <name>-$kver
        ubuntu-core-initramfs create-efi \
                              --kernelver "$kver" \
                              --initrd repacked-initrd \
                              --kernel vmlinuz \
                              --output repacked-kernel.efi

        mv "repacked-kernel.efi-$kver" kernel.efi

        # XXX: needed?
        chmod +x kernel.efi

        rm -rf unpacked-initrd skeleton initrd repacked-initrd-* vmlinuz-*
    )

    # drop ~450MB+ of firmware which should not be needed in qemu or the cloud system
    rm -rf repacked-kernel/firmware/*

    # copy any extra files that tests may need for the kernel
    if [ -d ./extra-kernel-snap/ ]; then
        cp -a ./extra-kernel-snap/* ./repacked-kernel
    fi
    
    snap pack repacked-kernel "$TARGET"
    rm -rf repacked-kernel
}


# Modify kernel and create a component, kernel content expected in pc-kernel
move_module_to_component() {
    mod_name=$1
    comp_name=$2

    kern_ver=$(find pc-kernel/modules/* -maxdepth 0 -printf "%f\n")
    comp_ko_dir=$comp_name/modules/"$kern_ver"/kmod/
    mkdir -p "$comp_ko_dir"
    mkdir -p "$comp_name"/meta/
    cat << EOF > "$comp_name"/meta/component.yaml
component: pc-kernel+$comp_name
type: kernel-modules
version: 1.0
summary: kernel component
description: kernel component for testing purposes
EOF
    # Replace _ or - with [_-], as it can be any of these
    glob_mod_name=$(printf '%s' "$mod_name" | sed -r 's/[-_]/[-_]/g')
    module_path=$(find pc-kernel -name "${glob_mod_name}.ko*")
    cp "$module_path" "$comp_ko_dir"
    snap pack --filename=pc-kernel+"$comp_name".comp "$comp_name"

    # remove the kernel module from the kernel snap
    rm "$module_path"
    # depmod wants a lib subdir
    mkdir -p pc-kernel/lib
    ln -s ../modules pc-kernel/lib/modules
    depmod -b pc-kernel/ "$kern_ver"
    # append component meta-information
    #shellcheck disable=SC2016
    gojq --arg COMP_NAME "${comp_name}" '.components = {$COMP_NAME:{"type":"kernel-modules"}}' --yaml-input pc-kernel/meta/snap.yaml --yaml-output >pc-kernel/meta/snap.yaml.new
    mv pc-kernel/meta/snap.yaml.new pc-kernel/meta/snap.yaml
}

uc24_build_initramfs_kernel_snap() {
    local ORIG_SNAP="$1"
    local TARGET="$2"
    local OPT_ARG="${3:-}"

    injectKernelPanic=false
    case "$OPT_ARG" in
        --inject-kernel-panic-in-initramfs)
            injectKernelPanic=true
            ;;
    esac

    unsquashfs -d pc-kernel "$ORIG_SNAP"
    kernelver=$(find pc-kernel/modules/ -maxdepth 1 -mindepth 1 -printf "%f")
    ubuntu-core-initramfs create-initrd --kernelver="$kernelver" --kerneldir pc-kernel/modules/"$kernelver" \
                          --firmwaredir pc-kernel/firmware --output initrd.img

    # Check that manifest is generated
    stat manifest-initramfs.yaml-"$kernelver"
    initrd_f=initrd.img-"$kernelver"
    unmkinitramfs "$initrd_f" initrd

    if [ -d ./extra-initrd ]; then
        if [ -d ./initrd/early ]; then
            cp -aT ./extra-initrd ./initrd/main
        else
            cp -aT ./extra-initrd ./initrd
        fi
    fi

    if [ -d ./initrd/early ]; then
        uc_write_bootstrap_wrapper ./initrd/main "$injectKernelPanic"

        (cd ./initrd/early; find . | cpio --create --quiet --format=newc --owner=0:0) >"$initrd_f"
        (cd ./initrd/main; find . | cpio --create --quiet --format=newc --owner=0:0 | zstd -1 -T0) >>"$initrd_f"
    else
        uc_write_bootstrap_wrapper ./initrd "$injectKernelPanic"

        (cd ./initrd; find . | cpio --create --quiet --format=newc --owner=0:0 | zstd -1 -T0) >"$initrd_f"
    fi

    # Build signed uki image - snakeoil keys shipped by ubuntu-core-initramfs
    # are used by default
    objcopy -O binary -j .linux pc-kernel/kernel.efi linux-"$kernelver"
    ubuntu-core-initramfs create-efi --kernelver="$kernelver" --initrd initrd.img --kernel linux --output kernel.efi
    cp kernel.efi-"$kernelver" pc-kernel/kernel.efi

    # copy any extra files that tests may need for the kernel
    if [ -d ./extra-kernel-snap/ ]; then
        cp -a ./extra-kernel-snap/* ./pc-kernel
    fi

    if [ -n "${NESTED_KERNEL_REMOVE_COMPONENTS-}" ]; then
        #shellcheck disable=SC2016
        gojq 'del(.components)' --yaml-input pc-kernel/meta/snap.yaml --yaml-output >pc-kernel/meta/snap.yaml.new
        mv pc-kernel/meta/snap.yaml.new pc-kernel/meta/snap.yaml
        gojq 'del(.slots)' --yaml-input pc-kernel/meta/snap.yaml --yaml-output >pc-kernel/meta/snap.yaml.new
        mv pc-kernel/meta/snap.yaml.new pc-kernel/meta/snap.yaml
    fi

    if [ -n "$NESTED_KERNEL_MODULES_COMP" ] && is_test_target_core_ge 24; then
        # "split" kernel in kernel-modules component and kernel
        move_module_to_component "$NESTED_COMP_KERNEL_MODULE_NAME" "$NESTED_KERNEL_MODULES_COMP"
    fi

    snap pack pc-kernel
    if [ "$(pwd)" != "$TARGET" ]; then
        mv pc-kernel_*.snap "$TARGET"
    fi
    rm -rf pc-kernel
}

setup_core_for_testing_by_modify_writable() {
    UNPACK_DIR="$1"

    # create test user and ubuntu user inside the writable partition
    # so that we can use a stock core in tests
    mkdir -p /mnt/user-data/test

    # create test user, see the comment in spread.yaml about 12345
    mkdir -p /mnt/system-data/etc/sudoers.d/
    echo 'test ALL=(ALL) NOPASSWD:ALL' >> /mnt/system-data/etc/sudoers.d/99-test-user
    echo 'ubuntu ALL=(ALL) NOPASSWD:ALL' >> /mnt/system-data/etc/sudoers.d/99-ubuntu-user
    # modify sshd so that we can connect as root
    mkdir -p /mnt/system-data/etc/ssh
    cp -a "$UNPACK_DIR"/etc/ssh/* /mnt/system-data/etc/ssh/
    # core18 is different here than core16
    sed -i 's/\#\?\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /mnt/system-data/etc/ssh/sshd_config
    # ensure the setting is correct
    grep '^PermitRootLogin yes' /mnt/system-data/etc/ssh/sshd_config

    # build the user database - this is complicated because:
    # - spread on linode wants to login as "root"
    # - "root" login on the stock core snap is disabled
    # - uids between classic/core differ
    # - passwd,shadow on core are read-only
    # - we cannot add root to extrausers as system passwd is searched first
    # - we need to add our ubuntu and test users too
    # So we create the user db we need in /root/test-etc/*:
    # - take core passwd without "root"
    # - append root
    # - make sure the group matches
    # - bind mount /root/test-etc/* to /etc/* via custom systemd job
    # We also create /var/lib/extrausers/* and append ubuntu,test there
    test ! -e /mnt/system-data/root
    mkdir -m 700 /mnt/system-data/root
    test -d /mnt/system-data/root
    mkdir -p /mnt/system-data/root/test-etc
    mkdir -p /mnt/system-data/var/lib/extrausers/
    touch /mnt/system-data/var/lib/extrausers/sub{uid,gid}
    mkdir -p /mnt/system-data/etc/systemd/system/multi-user.target.wants
    for f in group gshadow passwd shadow; do
        # the passwd from core without root
        grep -v "^root:" "$UNPACK_DIR/etc/$f" > /mnt/system-data/root/test-etc/"$f"
        # append this systems root user so that linode can connect
        grep "^root:" /etc/"$f" >> /mnt/system-data/root/test-etc/"$f"

        # make sure the group is as expected
        chgrp --reference "$UNPACK_DIR/etc/$f" /mnt/system-data/root/test-etc/"$f"
        # now bind mount read-only those passwd files on boot
        cat >/mnt/system-data/etc/systemd/system/etc-"$f".mount <<EOF
[Unit]
Description=Mount root/test-etc/$f over system etc/$f
Before=ssh.service

[Mount]
What=/root/test-etc/$f
Where=/etc/$f
Type=none
Options=bind,ro

[Install]
WantedBy=multi-user.target
EOF
        ln -s /etc/systemd/system/etc-"$f".mount /mnt/system-data/etc/systemd/system/multi-user.target.wants/etc-"$f".mount

        # create /var/lib/extrausers/$f
        # append ubuntu, test user for the testing
        grep "^test:" /etc/$f >> /mnt/system-data/var/lib/extrausers/"$f"
        grep "^ubuntu:" /etc/$f >> /mnt/system-data/var/lib/extrausers/"$f"
        # check test was copied
        MATCH "^test:" </mnt/system-data/var/lib/extrausers/"$f"
        MATCH "^ubuntu:" </mnt/system-data/var/lib/extrausers/"$f"
    done

    # Make sure systemd-journal group has the "test" user as a member. Due to the way we copy that from the host
    # and merge it from the core snap this is done explicitly as a second step.
    sed -r -i -e 's/^systemd-journal:x:([0-9]+):$/systemd-journal:x:\1:test/' /mnt/system-data/root/test-etc/group

    # ensure spread -reuse works in the core image as well
    if [ -e /.spread.yaml ]; then
        cp -av /.spread.yaml /mnt/system-data
    fi

    # using symbolic names requires test:test have the same ids
    # inside and outside which is a pain (see 12345 above), but
    # using the ids directly is the wrong kind of fragile
    chown --verbose test:test /mnt/user-data/test

    # we do what sync-dirs is normally doing on boot, but because
    # we have subdirs/files in /etc/systemd/system (created below)
    # the writeable-path sync-boot won't work
    mkdir -p /mnt/system-data/etc/systemd

    mkdir -p /mnt/system-data/var/lib/console-conf

    # NOTE: The here-doc below must use tabs for proper operation.
    cat >/mnt/system-data/etc/systemd/system/var-lib-systemd-linger.mount <<-UNIT
	[Mount]
	What=/writable/system-data/var/lib/systemd/linger
	Where=/var/lib/systemd/linger
	Options=bind
	UNIT
    ln -s /etc/systemd/system/var-lib-systemd-linger.mount /mnt/system-data/etc/systemd/system/multi-user.target.wants/var-lib-systemd-linger.mount

    # NOTE: The here-doc below must use tabs for proper operation.
    mkdir -p /mnt/system-data/etc/systemd/system/systemd-logind.service.d
    cat >/mnt/system-data/etc/systemd/system/systemd-logind.service.d/linger.conf <<-CONF
	[Service]
	StateDirectory=systemd/linger
	CONF

    (cd /tmp ; unsquashfs -no-progress -v  /var/lib/snapd/snaps/"$core_name"_*.snap etc/systemd/system)
    cp -avr /tmp/squashfs-root/etc/systemd/system /mnt/system-data/etc/systemd/
}

setup_reflash_magic() {
    # install the stuff we need
    distro_install_package kpartx busybox-static

    # Ensure we don't have snapd already installed, sometimes
    # on 20.04 purge seems to fail, catch that for further
    # debugging
    if [ -e /var/lib/snapd/state.json ]; then
        echo "reflash image not pristine, snaps already installed"
        python3 -m json.tool < /var/lib/snapd/state.json
        exit 1
    fi

    distro_install_local_package "$GOHOME"/snapd_*.deb
    distro_clean_package_cache

    # need to be seeded to proceed with snap install
    snap wait system seed.loaded

    # we cannot use "snaps.names tool" here because no snaps are installed yet
    core_name="core"
    if is_test_target_core 18; then
        core_name="core18"
    elif is_test_target_core 20; then
        core_name="core20"
    elif is_test_target_core 22; then
        core_name="core22"
    elif is_test_target_core 24; then
        core_name="core24"
    fi
    # XXX: we get "error: too early for operation, device not yet
    # seeded or device model not acknowledged" here sometimes. To
    # understand that better show some debug output.
    snap changes
    snap tasks --last=seed || true
    journalctl -u snapd
    snap model --verbose
    # remove the above debug lines once the mentioned bug is fixed
    snap install "--channel=${CORE_CHANNEL}" "$core_name"
    # TODO set up a trap to clean this up properly?
    local UNPACK_DIR
    UNPACK_DIR="$(mktemp -d "/tmp/$core_name-unpack.XXXXXXXX")"
    unsquashfs -no-progress -f -d "$UNPACK_DIR" /var/lib/snapd/snaps/${core_name}_*.snap

    if os.query is-arm; then
        snap install ubuntu-image --channel="$UBUNTU_IMAGE_SNAP_CHANNEL" --classic
    elif is_test_target_core 16; then
        # the new ubuntu-image expects mkfs to support -d option, which was not
        # supported yet by the version of mkfs that shipped with Ubuntu 16.04
        snap install ubuntu-image --channel="$OLD_UBUNTU_IMAGE_SNAP_CHANNEL" --classic
    else
        # shellcheck source=tests/lib/image.sh
        . "$TESTSLIB/image.sh"
        get_ubuntu_image
    fi

    # needs to be under /home because ubuntu-device-flash
    # uses snap-confine and that will hide parts of the hostfs
    IMAGE_HOME=/home/image
    IMAGE=pc.img
    mkdir -p "$IMAGE_HOME"

    # ensure that ubuntu-image is using our test-build of snapd with the
    # test keys and not the bundled version of usr/bin/snap from the snap.
    # Note that we can not put it into /usr/bin as '/usr' is different
    # when the snap uses confinement.
    cp /usr/bin/snap "$IMAGE_HOME"
    export UBUNTU_IMAGE_SNAP_CMD="$IMAGE_HOME/snap"

    if is_test_target_core 18; then
        build_snapd_snap "${IMAGE_HOME}"
        # FIXME: fetch directly once its in the assertion service
        cp "$TESTSLIB/assertions/ubuntu-core-18-amd64.model" "$IMAGE_HOME/pc.model"
    elif is_test_target_core 20; then
        build_snapd_snap_with_run_mode_firstboot_tweaks "$IMAGE_HOME"
        cp "$TESTSLIB/assertions/ubuntu-core-20-amd64.model" "$IMAGE_HOME/pc.model"
    elif is_test_target_core 22; then
        build_snapd_snap_with_run_mode_firstboot_tweaks "$IMAGE_HOME"
        if os.query is-arm; then
            cp "$TESTSLIB/assertions/ubuntu-core-22-arm64.model" "$IMAGE_HOME/pc.model"
        else
            cp "$TESTSLIB/assertions/ubuntu-core-22-amd64.model" "$IMAGE_HOME/pc.model"
        fi
    elif is_test_target_core 24; then
        build_snapd_snap_with_run_mode_firstboot_tweaks "$IMAGE_HOME"
        if os.query is-arm; then
            cp "$TESTSLIB/assertions/ubuntu-core-24-arm64.model" "$IMAGE_HOME/pc.model"
        else
            cp "$TESTSLIB/assertions/ubuntu-core-24-amd64.model" "$IMAGE_HOME/pc.model"
        fi
        
    else
        # FIXME: install would be better but we don't have dpkg on
        #        the image
        # unpack our freshly build snapd into the new snapd snap
        dpkg-deb -x "$GOHOME"/snapd_*.deb "$UNPACK_DIR"
        # Debian packages don't carry permissions correctly and we use
        # post-inst hooks to fix that on classic systems. Here, as a special
        # case, fix the void directory we just unpacked.
        chmod 111 "$UNPACK_DIR/var/lib/snapd/void"
        # ensure any new timer units are available
        cp -a /etc/systemd/system/timers.target.wants/*.timer "$UNPACK_DIR/etc/systemd/system/timers.target.wants"

        # add gpio and iio slots
        cat >> "$UNPACK_DIR/meta/snap.yaml" <<-EOF
slots:
    gpio-pin:
        interface: gpio
        number: 100
        direction: out
    iio0:
        interface: iio
        path: /dev/iio:device0
EOF

        # Make /var/lib/systemd writable so that we can get linger enabled.
        # This only applies to Ubuntu Core 16 where individual directories were
        # writable. In Core 18 and beyond all of /var/lib/systemd is writable.
        mkdir -p "$UNPACK_DIR"/var/lib/systemd/{catalog,coredump,deb-systemd-helper-enabled,rfkill,linger}
        touch "$UNPACK_DIR"/var/lib/systemd/random-seed

        # build new core snap for the image
        snap pack "$UNPACK_DIR" "$IMAGE_HOME"

        # FIXME: fetch directly once its in the assertion service
        cp "$TESTSLIB/assertions/pc-${REMOTE_STORE}.model" "$IMAGE_HOME/pc.model"
    fi

    EXTRA_FUNDAMENTAL=
    IMAGE_CHANNEL=
    if [ "$KERNEL_CHANNEL" = "$GADGET_CHANNEL" ]; then
        IMAGE_CHANNEL="$KERNEL_CHANNEL"
    else
        IMAGE_CHANNEL="$GADGET_CHANNEL"
        if is_test_target_core_le 18; then
            if is_test_target_core 16; then
                BRANCH=latest
            elif is_test_target_core 18; then
                BRANCH=18
            fi
            # download pc-kernel snap for the specified channel and set
            # ubuntu-image channel to that of the gadget, so that we don't
            # need to download it. Do this only for UC16/18 as the UC20+
            # case is considered a few lines below.
            snap download --basename=pc-kernel --channel="$BRANCH/$KERNEL_CHANNEL" pc-kernel
            # Repack to prevent reboots as the image channel (which will become
            # the tracked channel) is different to the kernel channel.
            unsquashfs -d pc-kernel pc-kernel.snap
            touch pc-kernel/repacked
            snap pack --filename=pc-kernel-repacked.snap pc-kernel
            rm -rf pc-kernel
            mv pc-kernel-repacked.snap pc-kernel.snap
            EXTRA_FUNDAMENTAL="--snap $PWD/pc-kernel.snap"
        fi
    fi

    if is_test_target_core_ge 20; then
        if is_test_target_core 20; then
            BRANCH=20
        elif is_test_target_core 22; then
            BRANCH=22
        elif is_test_target_core 24; then
            BRANCH=24
        fi
        snap download --basename=pc-kernel --channel="${BRANCH}/${KERNEL_CHANNEL}" pc-kernel
        # make sure we have the snap
        test -e pc-kernel.snap
        # build the initramfs with our snapd assets into the kernel snap
        if is_test_target_core_ge 24; then
            build_and_install_initramfs_deb
            uc24_build_initramfs_kernel_snap "$PWD/pc-kernel.snap" "$IMAGE_HOME"
        else
            uc20_build_initramfs_kernel_snap "$PWD/pc-kernel.snap" "$IMAGE_HOME"
        fi
        EXTRA_FUNDAMENTAL="--snap $IMAGE_HOME/pc-kernel_*.snap"

        # also add debug command line parameters to the kernel command line via
        # the gadget in case things go side ways and we need to debug
        snap download --basename=pc --channel="${BRANCH}/${KERNEL_CHANNEL}" pc
        test -e pc.snap
        unsquashfs -d pc-gadget pc.snap
        # TODO: it would be desirable when we need to do in-depth debugging of
        # UC20 runs in google to have snapd.debug=1 always on the kernel command
        # line, but we can't do this universally because the logic for the env
        # variable SNAPD_DEBUG=0|false does not overwrite the turning on of 
        # debug messages in some places when the kernel command line is set, so
        # we get failing tests since there is extra stuff on stderr than 
        # expected in the test when SNAPD_DEBUG is turned off
        # so for now, don't include snapd.debug=1, but eventually it would be
        # nice to have this on

        if [[ "$SPREAD_BACKEND" =~ google ]] || [[ "$SPREAD_BACKEND" =~ openstack ]]; then
            # the default console settings for snapd aren't super useful in GCE,
            # instead it's more useful to have all console go to ttyS0 which we 
            # can read more easily than tty1 for example
            for cmd in "console=ttyS0" "dangerous" "systemd.journald.forward_to_console=1" "rd.systemd.journald.forward_to_console=1" "panic=-1"; do
                echo "$cmd" >> pc-gadget/cmdline.full
            done
        else
            # but for other backends, just add the additional debugging things
            # on top of whatever the gadget currently is configured to use
            for cmd in "dangerous" "systemd.journald.forward_to_console=1" "rd.systemd.journald.forward_to_console=1"; do
                echo "$cmd" >> pc-gadget/cmdline.extra
            done
        fi

        # TODO: this probably means it's time to move this helper out of 
        # nested.sh to somewhere more general
        
        #shellcheck source=tests/lib/nested.sh
        . "$TESTSLIB/nested.sh"
        KEY_NAME=$(nested_get_snakeoil_key)

        SNAKEOIL_KEY="$PWD/$KEY_NAME.key"
        SNAKEOIL_CERT="$PWD/$KEY_NAME.pem"

        nested_secboot_sign_gadget pc-gadget "$SNAKEOIL_KEY" "$SNAKEOIL_CERT"
        snap pack --filename=pc-repacked.snap pc-gadget 
        mv pc-repacked.snap $IMAGE_HOME/pc-repacked.snap
        EXTRA_FUNDAMENTAL="$EXTRA_FUNDAMENTAL --snap $IMAGE_HOME/pc-repacked.snap"
    fi

    # 'snap pack' creates snaps 0644, and ubuntu-image just copies those in
    # maybe we should fix one or both of those, but for now this'll do
    chmod 0600 "$IMAGE_HOME"/*.snap

    # on core18 we need to use the modified snapd snap and on core16
    # it is the modified core that contains our freshly build snapd
    if is_test_target_core_ge 18; then
        extra_snap=("$IMAGE_HOME"/snapd_*.snap)
    else
        extra_snap=("$IMAGE_HOME"/core_*.snap)
    fi

    # extra_snap should contain only ONE snap
    if [ "${#extra_snap[@]}" -ne 1 ]; then
        echo "unexpected number of globbed snaps: ${extra_snap[*]}"
        exit 1
    fi

    # download the core20 snap manually from the specified channel for UC20
    if is_test_target_core_ge 20; then
        if is_test_target_core 20; then
            BASE=core20
        elif is_test_target_core 22; then
            BASE=core22
        elif is_test_target_core 24; then
            BASE=core24
        fi
        snap download "${BASE}" --channel="$BASE_CHANNEL" --basename="${BASE}"

        # we want to download the specific channel referenced by $BASE_CHANNEL, 
        # but if we just seed that revision and $BASE_CHANNEL != $IMAGE_CHANNEL,
        # then immediately on booting, snapd will refresh from the revision that
        # is seeded via $BASE_CHANNEL to the revision that is in $IMAGE_CHANNEL,
        # so to prevent that from happening (since that automatic refresh will 
        # confuse spread and make tests fail in awkward, confusing ways), we
        # unpack the snap and re-pack it so that it is not asserted and thus 
        # won't be automatically refreshed
        # note that this means that when $IMAGE_CHANNEL != $BASE_CHANNEL, we
        # will have unasserted snaps for all snaps on UC20 in GCE spread:
        # * snapd (to test the branch)
        # * pc-kernel (to test snap-bootstrap from the branch)
        # * pc (to aid in debugging by modifying the kernel command line)
        # * core20 (to avoid the automatic refresh issue)
        if [ "$IMAGE_CHANNEL" != "$BASE_CHANNEL" ]; then
            unsquashfs -d "${BASE}-snap" "${BASE}.snap"

            # We setup the ntp server in case it is defined in the current env
            # This is not needed in classic systems because the images already have ntp configured
            if [ -n "${NTP_SERVER:-}" ]; then
                TARGET_TIME_CONF="$(find "${BASE}-snap" -name timesyncd.conf)"
                if [ -z "$TARGET_TIME_CONF" ]; then
                    echo "File timesyncd.conf not found in core image"
                    exit 1
                fi
                cp /etc/systemd/timesyncd.conf "$TARGET_TIME_CONF"
            fi

            snap pack --filename="${BASE}-repacked.snap" "${BASE}-snap"
            rm -r "${BASE}-snap"
            mv "${BASE}-repacked.snap" "${IMAGE_HOME}/${BASE}.snap"
        else 
            mv "${BASE}.snap" "${IMAGE_HOME}/${BASE}.snap"
        fi

        EXTRA_FUNDAMENTAL="$EXTRA_FUNDAMENTAL --snap ${IMAGE_HOME}/${BASE}.snap"
    fi
    local UBUNTU_IMAGE="$GOHOME"/bin/ubuntu-image
    if is_test_target_core 16 || os.query is-arm; then
        # ubuntu-image on 16.04 needs to be installed from a snap
        UBUNTU_IMAGE=/snap/bin/ubuntu-image
    fi
    # shellcheck disable=SC2086
    "$UBUNTU_IMAGE" snap \
                    --image-size 5G \
                    -w "$IMAGE_HOME" "$IMAGE_HOME/pc.model" \
                    --channel "$IMAGE_CHANNEL" \
                    $EXTRA_FUNDAMENTAL \
                    --snap "${extra_snap[0]}" \
                    --output-dir "$IMAGE_HOME"
    rm -f ./pc-kernel_*.{snap,assert} ./pc-kernel.{snap,assert} ./pc_*.{snap,assert} ./snapd_*.{snap,assert} ./core{20,22,24}.{snap,assert}

    if os.query is-arm; then
        LOOP_PARTITION=1
    elif is_test_target_core_ge 20; then
        # (ab)use ubuntu-seed
        LOOP_PARTITION=2
    else
        LOOP_PARTITION=3
    fi

    # expand the uc16 and uc18 images a little bit (400M) as it currently will
    # run out of space easily from local spread runs if there are extra files in
    # the project not included in the git ignore and spread ignore, etc.
    if is_test_target_core_le 18; then
        # grow the image by 400M
        truncate --size=+400M "$IMAGE_HOME/$IMAGE"
        # fix the GPT table because old versions of parted complain about this 
        # and refuse to properly run the next command unless the GPT table is 
        # updated
        # this command moves the backup gpt partition to the end of the disk,
        # which is sensible since we've just resized the backing storage
        sgdisk "$IMAGE_HOME/$IMAGE" -e

        # resize the partition to go to the end of the disk
        parted -s "$IMAGE_HOME/$IMAGE" resizepart ${LOOP_PARTITION} "100%"
    fi

    # mount fresh image and add all our SPREAD_PROJECT data
    kpartx -avs "$IMAGE_HOME/$IMAGE"
    # losetup --list --noheadings returns:
    # /dev/loop1   0 0  1  1 /var/lib/snapd/snaps/ohmygiraffe_3.snap                0     512
    # /dev/loop57  0 0  1  1 /var/lib/snapd/snaps/http_25.snap                      0     512
    # /dev/loop19  0 0  1  1 /var/lib/snapd/snaps/test-snapd-netplan-apply_75.snap  0     512
    devloop=$(losetup --list --noheadings | grep "$IMAGE_HOME/$IMAGE" | awk '{print $1}')
    dev=$(basename "$devloop")

    # resize the 2nd partition from that loop device to fix the size
    if os.query is-core-le 18; then
        resize2fs -p "/dev/mapper/${dev}p${LOOP_PARTITION}"
    fi

    # mount it so we can use it now
    mount "/dev/mapper/${dev}p${LOOP_PARTITION}" /mnt

    # copy over everything from gopath to user-data, exclude:
    # - VCS files
    # - built debs
    # - golang archive files and built packages dir
    # - govendor .cache directory and the binary,
    if is_test_target_core_le 18; then
        mkdir -p /mnt/user-data/
        # we need to include "core" here because -C option says to ignore 
        # files the way CVS(?!) does, so it ignores files named "core" which
        # are core dumps, but we have a test suite named "core", so including 
        # this here will ensure that portion of the git tree is included in the
        # image
        rsync -a -C \
          --exclude '*.a' \
          --exclude '*.deb' \
          --exclude /gopath/.cache/ \
          --exclude /gopath/bin/govendor \
          --exclude /gopath/pkg/ \
          --include core/ \
          /home/gopath /mnt/user-data/
    else
        # prepare passwd for run-mode-overlay-data

        # use /etc/{group,passwd,shadow,gshadow} from the core20 snap, merged
        # with some bits from our current system - we don't want to use the
        # /etc/group from the current system as classic and core gids and uids
        # don't match, but we still need the same test/ubuntu/root user info
        # in core as we currently have in classic
        mkdir -p /root/test-etc
        mkdir -p /var/lib/extrausers
        touch /var/lib/extrausers/sub{uid,gid}
        for f in group gshadow passwd shadow; do
            grep -v "^root:" "$UNPACK_DIR/etc/$f" > /root/test-etc/"$f"
            grep "^root:" /etc/"$f" >> /root/test-etc/"$f"
            chgrp --reference "$UNPACK_DIR/etc/$f" /root/test-etc/"$f"
            # create /var/lib/extrausers/$f
            # append ubuntu, test user for the testing
            grep "^test:" /etc/"$f" >> /var/lib/extrausers/"$f"
            grep "^ubuntu:" /etc/"$f" >> /var/lib/extrausers/"$f"
            # check test was copied
            MATCH "^test:" </var/lib/extrausers/"$f"
            MATCH "^ubuntu:" </var/lib/extrausers/"$f"
        done
        # Make sure systemd-journal group has the "test" user as a member. Due
        # to the way we copy that from the host and merge it from the core snap
        # this is done explicitly as a second step.
        sed -r -i -e 's/^systemd-journal:x:([0-9]+):$/systemd-journal:x:\1:test/' /root/test-etc/group
        tar -c -z \
          --exclude '*.a' \
          --exclude '*.deb' \
          --exclude /gopath/.cache/ \
          --exclude /gopath/bin/govendor \
          --exclude /gopath/pkg/ \
          -f /mnt/run-mode-overlay-data.tar.gz \
          /home/gopath /root/test-etc /var/lib/extrausers
    fi

    # now modify the image writable partition - only possible on uc16 / uc18
    if is_test_target_core_le 18; then
        # modify the writable partition of "core" so that we have the
        # test user
        setup_core_for_testing_by_modify_writable "$UNPACK_DIR"
    fi

    # unmount the partition we just modified and delete the image's loop devices
    umount /mnt
    kpartx -d "$IMAGE_HOME/$IMAGE"

    if command -v pigz 2>/dev/null; then
        pigz "${IMAGE_HOME}/${IMAGE}"
    else
        gzip "${IMAGE_HOME}/${IMAGE}"
    fi

    if is_test_target_core 16; then
        "${TESTSLIB}/uc16-reflash.sh" "${IMAGE_HOME}/${IMAGE}.gz"
    else
        "${TESTSLIB}/reflash.sh" "${IMAGE_HOME}/${IMAGE}.gz"
    fi

    rm -rf "$UNPACK_DIR"
}

prepare_state_lock(){
    TAG=$1
    CONF_FILE="/etc/systemd/system/snapd.service.d/state-lock.conf"
    LOCKS_FILE="$TESTSTMP"/snapd_lock_traces
    RESTART=false

    if [ "$SNAPD_STATE_LOCK_TRACE_THRESHOLD_MS" -gt 0 ]; then
        echo "###START: $TAG" >> "$LOCKS_FILE"

        # Generate the config file when it does not exist and when the threshold has changed different
        if ! [ -f "$CONF_FILE" ] || ! grep -q "SNAPD_STATE_LOCK_TRACE_THRESHOLD_MS=$SNAPD_STATE_LOCK_TRACE_THRESHOLD_MS" < "$CONF_FILE"; then
            echo "Prepare snapd for getting state lock time"
            cat <<EOF > "$CONF_FILE"
[Service]
Environment=SNAPPY_TESTING=1
Environment=SNAPD_STATE_LOCK_TRACE_THRESHOLD_MS="$SNAPD_STATE_LOCK_TRACE_THRESHOLD_MS"
Environment=SNAPD_STATE_LOCK_TRACE_FILE="$LOCKS_FILE"
EOF
            RESTART=true
        fi
    elif [ -f "$CONF_FILE" ]; then
        rm -f "$CONF_FILE"
        RESTART=true
    fi

    if [ "$RESTART" = "true" ]; then
        # the service setting may have changed in the service so we need
        # to ensure snapd is reloaded
        systemctl daemon-reload
        systemctl restart snapd
    fi
}

prepare_tag_features(){
    CONF_FILE="/etc/systemd/system/snapd.service.d/99-feature-tags.conf"
    RESTART=false

    if [ -n "$TAG_FEATURES" ]; then
        # Generate the config file when it does not exist and when the threshold has changed different
        if ! [ -f "$CONF_FILE" ]; then
            cat <<EOF > "$CONF_FILE"
[Service]
Environment=SNAPPY_TESTING=1
Environment=SNAPD_TRACE=1
Environment=SNAPD_JSON_LOGGING=1
EOF
            RESTART=true
        fi
    elif [ -f "$CONF_FILE" ]; then
        rm -f "$CONF_FILE"
        RESTART=true
    fi

    if [ "$RESTART" = "true" ]; then
        # the service setting may have changed in the service so we need
        # to ensure snapd is reloaded
        systemctl daemon-reload
        systemctl restart snapd
    fi
}

# prepare_ubuntu_core will prepare ubuntu-core 16+
prepare_ubuntu_core() {
    # Configure the proxy in the system when it is required
    setup_system_proxy

    # we are still a "classic" image, prepare the surgery
    if [ -e /var/lib/dpkg/status ]; then
        setup_reflash_magic
        REBOOT
    fi
    setup_snapd_proxy

    disable_journald_rate_limiting
    disable_journald_start_limiting

    # verify after the first reboot that we are now in core18 world
    if [ "$SPREAD_REBOOT" = 1 ]; then
        echo "Ensure we are now in an all-snap world"
        if [ -e /var/lib/dpkg/status ]; then
            echo "Rebooting into all-snap system did not work"
            exit 1
        fi
    fi

    # Wait for the snap command to become available.
    if [ "$SPREAD_BACKEND" != "external" ] && [ "$SPREAD_BACKEND" != "testflinger" ]; then
        # shellcheck disable=SC2016
        retry -n 120 --wait 1 sh -c 'test "$(command -v snap)" = /usr/bin/snap && snap version | grep -E -q "snapd +1337.*"'
    fi

    # Wait for seeding to finish.
    snap wait system seed.loaded

    echo "Ensure fundamental snaps are still present"
    for name in "$(snaps.name gadget)" "$(snaps.name kernel)" "$(snaps.name core)"; do
        if ! snap list "$name"; then
            echo "Not all fundamental snaps are available, all-snap image not valid"
            echo "Currently installed snaps"
            snap list
            exit 1
        fi
    done

    echo "Ensure the snapd snap is available"
    if os.query is-core-ge 18; then
        if ! snap list snapd; then
            echo "snapd snap on core18 is missing"
            snap list
            exit 1
        fi
    fi

    echo "Ensure rsync is available"
    if ! command -v rsync; then
        rsync_snap="test-snapd-rsync"
        if os.query is-core18; then
            rsync_snap="test-snapd-rsync-core18"
        elif os.query is-core20; then
            rsync_snap="test-snapd-rsync-core20"
        elif os.query is-core22; then
            rsync_snap="test-snapd-rsync-core22"
        elif os.query is-core24; then
            rsync_snap="test-snapd-rsync-core24"
        elif os.query is-core26; then
            rsync_snap="test-snapd-rsync-core26"
        fi
        snap install --devmode --edge "$rsync_snap"
        snap alias "$rsync_snap".rsync rsync
    fi

    # Cache snaps
    # shellcheck disable=SC2086
    cache_snaps ${PRE_CACHE_SNAPS}

    echo "Ensure the core snap is cached"
    # Cache snaps
    if os.query is-core-ge 18; then
        if snap list core >& /dev/null; then
            echo "core snap on core18 should not be installed yet"
            snap list
            exit 1
        fi
        cache_snaps core
        if os.query is-core18; then
            cache_snaps test-snapd-sh-core18
        fi
        if os.query is-core20; then
            cache_snaps test-snapd-sh-core20
        fi
        if os.query is-core22; then
            cache_snaps test-snapd-sh-core22
        fi
        if os.query is-core24; then
            cache_snaps test-snapd-sh-core24
        fi
        if os.query is-core26; then
            cache_snaps test-snapd-sh-core26
        fi
    fi

    disable_refreshes
    setup_systemd_snapd_overrides

    # Snapshot the fresh state (including boot/bootenv)
    if ! is_snapd_state_saved; then
        # Create the file with the initial environment before saving the state
        tests.env start initial

        # save preinstalled snaps when tests are executed in external systems
        # the preinstalled snaps shouldn't be removed during tests clean up
        # this is needed just for external devices because those could be using
        # custom images with pre-installed snaps which cannot be removed, such
        # as the network-manager.
        if [ "$SPREAD_BACKEND" = "external" ] || [ "$SPREAD_BACKEND" = "testflinger" ]; then
            PREINSTALLED_SNAPS="$(snap list | tail -n +2 | awk '{print $1}' | tr '\n' ' ')"
            tests.env set initial PREINSTALLED_SNAPS "$PREINSTALLED_SNAPS"
        fi

        # important to remove disabled snaps before calling save_snapd_state
        # or restore will break
        remove_disabled_snaps
        prepare_memory_limit_override
        prepare_state_lock "SNAPD PROJECT"
        prepare_tag_features
        setup_experimental_features
        systemctl stop snapd.service snapd.socket
        save_snapd_state
        systemctl start snapd.socket
    fi

    disable_kernel_rate_limiting
}

cache_snaps(){
    # Pre-cache snaps so that they can be installed by tests quickly.
    # This relies on a behavior of snapd which snaps installed are
    # cached and then used when need to the installed again

    # Download each of the snaps we want to pre-cache. Note that `snap download`
    # a quick no-op if the file is complete.
    for snap_name in "$@"; do
        snap download "$snap_name"

        # Copy all of the snaps back to the spool directory. From there we
        # will reuse them during subsequent `snap install` operations.
        snap_file=$(ls "${snap_name}"_*.snap)
        mv "${snap_file}" /var/lib/snapd/snaps/"${snap_file}".partial
        rm -f "${snap_name}"_*.assert
    done
}
