set -e

# One pass per node: this is the prepare step of an operating system upgrade job. It runs before the node
# is cordoned and changes nothing that is in use: it puts the binary of the target Kubernetes version next
# to the running one. The upgrade step moves it into place just before it reboots the node.

HOST="${HOST:-/host}"
NEW_BINARY="${NEW_BINARY:-/opt/rke2}"
KUBERNETES_MARKER="${KUBERNETES_MARKER:-$HOST/run/elemental-onepass}"
KUBECTL="${KUBECTL:-kubectl}"
CRICTL="${CRICTL:-/var/lib/rancher/rke2/bin/crictl}"
KUBERNETES_VERSION="{{ .KubernetesVersion }}"
OS_IMAGE="{{ .OSImage }}"

# The image of this step is not the operating system image, so the node is asked to pull that image here:
# it should be present before the node is cordoned. The upgrade step pulls it if this did not work.
pull_os_image() {
    # The path may be a link that only resolves on the node itself.
    if ! chroot "$HOST" "$CRICTL" --version >/dev/null 2>&1; then
        echo "No crictl at $CRICTL on the node. The upgrade step pulls the OS image."
        return 0
    fi
    if chroot "$HOST" "$CRICTL" --runtime-endpoint unix:///run/k3s/containerd/containerd.sock pull "$OS_IMAGE" >/dev/null 2>&1; then
        echo "OS image is available on the node"
    else
        echo "The OS image could not be pulled in advance. The upgrade step pulls it."
    fi
}

# Prints major.minor.patch of a version as one number.
version_number() {
    echo "$1" | grep -Eo '[0-9]+\.[0-9]+\.[0-9]+' | head -n 1 | awk -F. '{ printf "%03d%03d%03d", $1, $2, $3 }'
}

if [ -z "$RKE2_BIN_PATH" ]; then
    RKE2_PID=$(ps -ef | grep -E "(/usr|/usr/local|/opt/rke2)/bin/rke2 .*(server|agent)" | grep -E -v "(init|grep)" | awk '{print $1}')
    if [ "$(echo $RKE2_PID | wc -w)" != "1" ]; then
        echo "ERROR: expected one running rke2 process on this node, found: '$RKE2_PID'"
        exit 1
    fi
    # rke2 rewrites its command line: the path and the arguments may be one string, separated by spaces.
    RKE2_BIN_PATH=$(tr '\0' '\n' < "$HOST/proc/$RKE2_PID/cmdline" | awk 'NR==1 {print $1}')
fi

TARGET_BIN="$HOST$RKE2_BIN_PATH"
STAGED="$TARGET_BIN.elemental-staged"
if [ -z "$RKE2_BIN_PATH" ] || [ ! -f "$TARGET_BIN" ] || [ ! -f "$NEW_BINARY" ]; then
    echo "ERROR: rke2 binary not found (on the node: '$RKE2_BIN_PATH', new: $NEW_BINARY)"
    exit 1
fi

# Nothing that an earlier attempt staged is used.
rm -f "$KUBERNETES_MARKER" "$STAGED" "$STAGED.tmp"

NEW_SUM=$(sha256sum "$NEW_BINARY" | awk '{print $1}')
CURRENT_SUM=$(sha256sum "$TARGET_BIN" | awk '{print $1}')
if [ "$NEW_SUM" = "$CURRENT_SUM" ]; then
    echo "The node already has the rke2 binary of $KUBERNETES_VERSION. Nothing is staged."
    pull_os_image
    exit 0
fi

NEW_NUMBER=$(version_number "$("$NEW_BINARY" -v | head -n 1)")
CURRENT_NUMBER=$(version_number "$("$TARGET_BIN" -v | head -n 1)")
if [ -n "$NEW_NUMBER" ] && [ -n "$CURRENT_NUMBER" ] && [ "$CURRENT_NUMBER" -gt "$NEW_NUMBER" ]; then
    echo "ERROR: the node runs a newer rke2 than $KUBERNETES_VERSION"
    exit 1
fi
{{ if .Worker }}
# A worker node must not run a newer Kubernetes than the control plane. The plan of the worker nodes is created when
# the control plane nodes are done; if they do not all report the target version here, they were not upgraded in one
# pass and the worker node is not either: the Kubernetes upgrade phase follows.
attempt=0
until CONTROL_PLANE_NODES=$($KUBECTL get nodes --selector='node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.status.nodeInfo.kubeletVersion}{"\n"}{end}'); do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 30 ]; then
        echo "ERROR: could not read the Kubernetes version of the control plane nodes"
        exit 1
    fi
    sleep 10
done
CONTROL_PLANE_VERSIONS=$(echo "$CONTROL_PLANE_NODES" | sort -u)
if [ "${CONTROL_PLANE_VERSIONS#v}" != "${KUBERNETES_VERSION#v}" ]; then
    echo "The control plane nodes report '$(echo $CONTROL_PLANE_VERSIONS)', not only $KUBERNETES_VERSION. Nothing is staged."
    pull_os_image
    exit 0
fi
{{ end }}
cp "$NEW_BINARY" "$STAGED.tmp"
chmod 755 "$STAGED.tmp"
CONTEXT=$(getfilecon "$TARGET_BIN" 2>/dev/null | awk '{print $2}' || true)
if [ -n "$CONTEXT" ]; then
    setfilecon "$CONTEXT" "$STAGED.tmp"
fi
mv -f "$STAGED.tmp" "$STAGED"

mkdir -p "$(dirname "$KUBERNETES_MARKER")"
echo "$RKE2_BIN_PATH $NEW_SUM $KUBERNETES_VERSION" > "$KUBERNETES_MARKER"
echo "Kubernetes $KUBERNETES_VERSION is staged at $RKE2_BIN_PATH.elemental-staged"

pull_os_image
