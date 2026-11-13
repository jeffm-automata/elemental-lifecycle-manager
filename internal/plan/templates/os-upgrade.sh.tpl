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

if [ -n "$CURRENT" ]; then
	# Extract the prefix (e.g. "uri: oci://") before the OS_IMAGE_REPO,
	# so that it can be stripped in the next step.
    prefix=${CURRENT%%"$OS_IMAGE_REPO"*}
	CURRENT=${CURRENT#"$prefix"}
    if [ "$CURRENT" = "$INCOMING" ]; then
        echo "Active OS image is already at correct version $OS_VERSION. Upgrade has been performed."
        exit 0
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
            exit 0
        fi
        ;;
esac

{{ if .FetchRemote }}USE_LOCAL_IMAGES=false {{ end }}upgrader "$INCOMING" && chroot /host reboot
