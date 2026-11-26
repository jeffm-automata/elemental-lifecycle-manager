set -e

HOST="${HOST:-/host}"
DEPLOYMENT="${DEPLOYMENT:-$HOST/etc/elemental/deployment.yaml}"
RELEASE_MARKER="${RELEASE_MARKER:-$HOST/etc/elemental/release.yaml}"
OS_IMAGE="{{ .OSImage }}"
OS_IMAGE_REPO="{{ .OSImageRepo }}"
OS_VERSION="{{ .OSImageVersion }}"
INCOMING="$OS_IMAGE_REPO:$OS_VERSION"
CURRENT=$(grep -F "uri: oci://$OS_IMAGE_REPO" "$DEPLOYMENT" 2>/dev/null || true)
SOURCE=$(grep -F "uri: " "$DEPLOYMENT" 2>/dev/null || true)
{{ if .OnePass }}
# One pass per node: the prepare step of this job may have put the binary of the target Kubernetes
# version next to the running one. It is recorded in a marker that does not survive a reboot.
KUBERNETES_MARKER="${KUBERNETES_MARKER:-$HOST/run/elemental-onepass}"

# A node that is not rebooted keeps its Kubernetes version: the Kubernetes upgrade phase handles it.
discard_staged_kubernetes() {
    [ -e "$KUBERNETES_MARKER" ] || return 0
    STAGED_BIN=""
    STAGED_VERSION=""
    read -r STAGED_BIN STAGED_SIZE STAGED_VERSION < "$KUBERNETES_MARKER" || true
    if [ -n "$STAGED_BIN" ]; then
        rm -f "$HOST$STAGED_BIN.elemental-staged"
    fi
    rm -f "$KUBERNETES_MARKER"
    echo "Kubernetes $STAGED_VERSION was staged, but the node is not rebooted. It is left to the Kubernetes upgrade phase."
}

# Puts the staged binary in place of the running one, so that the reboot into the new operating
# system starts the target Kubernetes version. The running process keeps the old binary until then.
install_staged_kubernetes() {
    if [ ! -s "$KUBERNETES_MARKER" ]; then
        echo "No Kubernetes binary is staged. The node keeps its Kubernetes version."
        return 0
    fi

    STAGED_BIN=""
    STAGED_SIZE=""
    STAGED_VERSION=""
    read -r STAGED_BIN STAGED_SIZE STAGED_VERSION < "$KUBERNETES_MARKER" || true
    TARGET_BIN="$HOST$STAGED_BIN"
    STAGED="$TARGET_BIN.elemental-staged"
    if [ -z "$STAGED_BIN" ] || [ ! -f "$STAGED" ] || [ ! -f "$TARGET_BIN" ]; then
        echo "ERROR: the staged Kubernetes binary for '$STAGED_BIN' is missing. The node is not rebooted."
        return 1
    fi

    if [ "$(wc -c < "$STAGED" | tr -d ' ')" != "$STAGED_SIZE" ]; then
        echo "ERROR: the staged Kubernetes binary $STAGED_BIN.elemental-staged is not the one that was staged. The node is not rebooted."
        return 1
    fi

    # The binary that is replaced stays on the node: the operating system snapshots do not contain it.
    ln -f "$TARGET_BIN" "$TARGET_BIN.elemental-previous"
    mv -f "$STAGED" "$TARGET_BIN"

    UNITS="$(dirname "$TARGET_BIN")/../lib/systemd/system"
    if [ -d "$UNITS" ]; then
        for component in server agent; do
            grep -sq '^HOME=' "$UNITS/rke2-$component.env" || printf '\nHOME=/root\n' >> "$UNITS/rke2-$component.env"
        done
    fi

    rm -f "$KUBERNETES_MARKER"
    sync
    echo "Kubernetes $STAGED_VERSION is installed at $STAGED_BIN. It starts with the reboot into the new operating system."
}
{{ end }}
if [ -n "$CURRENT" ]; then
	# Extract the prefix (e.g. "uri: oci://") before the OS_IMAGE_REPO,
	# so that it can be stripped in the next step.
    prefix=${CURRENT%%"$OS_IMAGE_REPO"*}
	CURRENT=${CURRENT#"$prefix"}
    if [ "$CURRENT" = "$INCOMING" ]; then
        echo "Active OS image is already at correct version $OS_VERSION. Upgrade has been performed."
{{ if .OnePass }}        discard_staged_kubernetes
{{ end }}        exit 0
    fi
fi

# On fresh systems, we have a sourceOS specified with raw (e.g. raw://../squashfs.img) data
# instead of from an OCI image, so the deployment does not tell which OS image the system runs.
# Images built by 'elemental3 customize' record that in a release marker, which names the
# OS image exactly as the release manifest that the image was built from does.
#
# The marker describes the installation only, so it is ignored as soon as the deployment refers
# to an OCI image. Without a marker, or with one that names any other image, we assume that the
# system has to be upgraded.
case "$SOURCE" in
    "" | *"uri: oci://"*) ;;
    *)
        INSTALLED=$(sed -n 's/^osImage:[[:space:]]*\(.*[^[:space:]]\)[[:space:]]*$/\1/p' "$RELEASE_MARKER" 2>/dev/null | head -n 1)
        if [ -n "$INSTALLED" ] && [ "$INSTALLED" = "$OS_IMAGE" ]; then
            echo "System was installed from OS image $OS_IMAGE. No upgrade needed."
{{ if .OnePass }}            discard_staged_kubernetes
{{ end }}            exit 0
        fi
        ;;
esac

{{ if .FetchRemote }}USE_LOCAL_IMAGES=false {{ end }}upgrader "$INCOMING"{{ if .OnePass }} && install_staged_kubernetes{{ end }} && chroot /host reboot
