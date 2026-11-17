#!/usr/bin/python3
"""Checks what extract-in-container.sh took out of a node image. Standard library only.

  inspect.py <extract-dir> <report.txt> <result.json> [<split-config-dir>]

Exit 0: every check passed. Exit 1: at least one failed. The report never prints file contents that
matched a secret pattern, only the path and the pattern name.
"""
import base64, gzip, json, os, re, sys, urllib.parse

X, REPORT, RESULT = sys.argv[1], sys.argv[2], sys.argv[3]
SPLIT = sys.argv[4] if len(sys.argv) > 4 and sys.argv[4] else None
out, checks = [], []


def say(s=""):
    out.append(s)


def check(cid, passed, detail):
    checks.append({"id": cid, "passed": bool(passed), "detail": detail})
    say(f"[{'PASS' if passed else 'FAIL'}] {cid}: {detail}")


def read(path, default=None):
    try:
        with open(os.path.join(X, path), errors="replace") as f:
            return f.read()
    except OSError:
        return default


def decode(entry):
    src = entry.get("contents", {}).get("source")
    if src is None:
        return ""
    if not src.startswith("data:"):
        return f"<remote source {src}>"
    head, _, body = src[5:].partition(",")
    data = base64.b64decode(body) if head.endswith(";base64") else urllib.parse.unquote_to_bytes(body)
    if entry["contents"].get("compression") == "gzip":
        data = gzip.decompress(data)
    return data.decode(errors="replace")


# ---- the three places content can come from ------------------------------------------------
# 1. Ignition base config that customize generates (applied on the node's first boot)
ign_files, ign_units, ign_other = {}, {}, {}
base_d = os.path.join(X, "initrd-ext/usr/lib/ignition/base.d")
for name in sorted(os.listdir(base_d)) if os.path.isdir(base_d) else []:
    cfg = json.load(open(os.path.join(base_d, name)))
    for f in cfg.get("storage", {}).get("files", []):
        ign_files[f["path"]] = decode(f)
    for u in cfg.get("systemd", {}).get("units", []):
        ign_units[u["name"]] = u.get("contents", "") + "".join(d.get("contents", "") for d in u.get("dropins", []))
    for key in ("passwd", "kernelArguments"):
        if cfg.get(key):
            ign_other[key] = cfg[key]
    for key in ("links", "directories", "filesystems", "disks", "luks", "raid"):
        if cfg.get("storage", {}).get(key):
            ign_other["storage." + key] = cfg["storage"][key]
    if cfg.get("ignition", {}).get("config"):
        ign_other["ignition.config"] = cfg["ignition"]["config"]
# 2. Files customize lays over the installed system, and the installer's own files
rec_files = [l.split(" ", 2) for l in (read("recovery/files.txt") or "").splitlines()]
overlay = sorted(p[len("./Install/Overlay"):] for t, _, p in rec_files if t != "d" and p.startswith("./Install/Overlay/"))
# 3. The OS that is installed (the published OS image, as shipped in the installer)
os_list = set()
for line in (read("os/files.txt") or "").splitlines():
    m = re.search(r"squashfs-root(/\S*)", line)
    if m:
        os_list.add(m.group(1))
cmdline_installer = re.search(r"^cmdline=(.*)$", read("efi/boot/grubenv") or "", re.M)
cmdline_installer = cmdline_installer.group(1).strip("# \n") if cmdline_installer else ""
install_yaml = read("recovery/Install/install.yaml") or ""
cmdline_system = re.search(r"^bootloader:.*?kernelCmdline: *(.*?)$", install_yaml, re.M | re.S)
cmdline_system = cmdline_system.group(1) if cmdline_system else ""

say("Image layout")
say((read("partitions.txt") or "").strip())
say((read("filesystems.txt") or "").strip())
say()
say(f"Installer kernel command line: {cmdline_installer}")
say(f"Installed system kernel command line: {cmdline_system}")
say()
say(f"Files written at first boot by the built-in Ignition config ({len(ign_files)}):")
for p in sorted(ign_files):
    say(f"  {p} ({len(ign_files[p])} bytes)")
say(f"Units from the built-in Ignition config: {', '.join(sorted(ign_units))}")
say(f"Other Ignition sections: {json.dumps(ign_other) if ign_other else 'none'}")
say()
say(f"Files laid over the installed system ({len(overlay)}):")
for p in overlay:
    say(f"  {p}")
say()
say(f"OS to install: {len(os_list)} paths in LiveOS/squashfs.img")
say()

# ---- checks ------------------------------------------------------------------------------
everything = {**{"ignition:" + p: c for p, c in ign_files.items()},
              **{"ignition-unit:" + n: c for n, c in ign_units.items()}}
for t, _, p in rec_files:  # small recovery files were copied out by the extraction
    c = read(os.path.join("recovery", p)) if t == "f" else None
    if c is not None:
        everything["recovery:" + p[1:]] = c


def present(paths_re):
    """Paths matching the regex in any of the three sources."""
    r = re.compile(paths_re)
    return sorted(["ignition:" + p for p in ign_files if r.search(p)] +
                  ["overlay:" + p for p in overlay if r.search(p)] +
                  ["os:" + p for p in os_list if r.search(p)])


rke2_cfg = {p: c for p, c in ign_files.items() if re.search(r"/kubernetes/(init|server|agent)\.yaml$", p)}
tok = [p for p, c in rke2_cfg.items() if re.search(r"^\s*(token|agent-token|token-file)\s*:", c, re.M)]
secret_pat = {
    "rke2-token-key": r"^\s*(token|agent-token)\s*:\s*\S",
    "k10-token": r"\bK10[0-9a-f]{40,}",
    "private-key": r"-----BEGIN [A-Z ]*PRIVATE KEY-----",
    "password-assignment": r"(?i)\bpassword\s*[:=]\s*[\"']?[^\s\"'$<{][^\s\"']{3,}",
    "registry-auth": r"(?i)\"?auth\"?\s*:\s*\"[A-Za-z0-9+/=]{8,}\"",
    "bearer": r"(?i)bearer\s+[A-Za-z0-9._-]{16,}",
}
hits = sorted(f"{src} [{name}]" for src, c in everything.items() for name, pat in secret_pat.items()
              if re.search(pat, c, re.M) and not src.endswith("/opt/k8s/install/install.sh"))
check("no-cluster-token", rke2_cfg and not tok and not [h for h in hits if "token" in h],
      f"{len(rke2_cfg)} base RKE2 configs ({', '.join(sorted(os.path.basename(p) for p in rke2_cfg))}), "
      f"token keys in: {tok or 'none'}")
srv = [p for p, c in rke2_cfg.items() if re.search(r"^\s*(server|tls-san|node-name|node-ip|advertise-address)\s*:", c, re.M)]
check("no-cluster-address", not srv, f"server/tls-san/node-name/node-ip keys in base RKE2 configs: {srv or 'none'}")
check("no-other-secret", not hits, f"secret patterns in {len(everything)} generated text files: {hits or 'none'}")
role = present(r"/elemental/runtime\.env$")
deploy = next((c for p, c in ign_files.items() if p.endswith("k8s_conf_deploy.sh")), "")
static_nodes = re.findall(r"^hosts\[[^\]]+\]=\S+", deploy, re.M)
check("no-node-role", not role and not static_nodes,
      f"runtime.env: {role or 'absent'}; nodes named in the deploy script: {static_nodes or 'none'}")
hn = present(r"^/etc/hostname$")
os_hostname = (read("os/root/etc/hostname") or "").strip()
check("no-hostname", not os_hostname and not [h for h in hn if not h.startswith("os:")],
      f"/etc/hostname: {hn or 'absent'}; content in the OS: {os_hostname!r}")
mid = present(r"^/(etc|var/lib/dbus)/machine-id$")
os_mid = read("os/root/etc/machine-id")
ok_mid = os_mid is None or os_mid.strip() in ("", "uninitialized")
mid_elsewhere = [m for m in mid if not m.startswith("os:")]
check("no-machine-id", ok_mid and not mid_elsewhere,
      f"/etc/machine-id in the OS: {'absent' if os_mid is None else 'empty or uninitialized' if ok_mid else 'SET'}; "
      f"written by Ignition or the overlay: {mid_elsewhere or 'no'}; /var/lib/dbus/machine-id is a link to /etc/machine-id")
hk = present(r"^/etc/ssh/ssh_host_.*key")
check("no-ssh-host-keys", not hk, f"/etc/ssh/ssh_host_*: {hk or 'none'}")
ak = present(r"/(authorized_keys|authorized_keys2|id_(rsa|ed25519|ecdsa)(\.pub)?)$")
check("no-ssh-user-keys", not ak and "passwd" not in ign_other,
      f"authorized_keys or key pairs: {ak or 'none'}; Ignition passwd section: {'present' if 'passwd' in ign_other else 'absent'}")
shadow = read("os/root/etc/shadow") or ""
unlocked = sorted(l.split(":")[0] for l in shadow.splitlines() if l.count(":") > 1 and not re.match(r"^[!*]", l.split(":")[1]))
check("no-login-password", not unlocked, f"accounts with a usable or empty password in the OS: {unlocked or 'none'} "
      f"({len(shadow.splitlines())} accounts, all locked)" if not unlocked else f"unlocked accounts: {unlocked}")
net = present(r"^/etc/(NetworkManager/system-connections/.+|sysconfig/network/ifcfg-(?!lo$).+|systemd/network/.+\.(network|netdev|link))$")
net_cmd = re.findall(r"\b(?:ip|ifname|hostname|nameserver|rd\.neednet|bootdev|BOOTIF|vlan|bond|team|bridge)=\S+",
                     cmdline_installer + " " + cmdline_system)
check("no-network-identity", not net and not net_cmd,
      f"connection profiles: {net or 'none'}; network kernel parameters: {net_cmd or 'none'}")
k8s_state = present(r"^/(etc/rancher/|var/lib/rancher/|var/lib/kubelet/|etc/cni/).+")
check("no-cluster-state", not k8s_state, f"/etc/rancher, /var/lib/rancher, kubelet or CNI state: {k8s_state[:5] or 'none'}")
reg = ign_files.get(next((p for p in ign_files if p.endswith("/kubernetes/registries.yaml")), ""), "")
check("no-registry-credentials", not re.search(r"(?i)\b(auth|username|password|token)\b", reg),
      f"registries.yaml: {reg.strip() or 'absent'}")
hook = ign_files.get("/opt/install.sh", "")
check("bootstrap-install-hook", "INSTALL_RKE2_TYPE" in hook, "/opt/install.sh " + ("present, takes INSTALL_RKE2_TYPE" if hook else "ABSENT"))
ci = sorted(u for u in ign_units if "cloud-init" in u)
platform_id = re.search(r"ignition\.platform\.id=(\S+)", cmdline_system)
platform_id = platform_id.group(1) if platform_id else "not set"
check("cloud-config-user-data", len(ci) >= 3 and platform_id == "metal",
      f"cloud-init units: {ci or 'none'}; ignition.platform.id in the kernel command line: {platform_id}")
marker = read("recovery/Install/Overlay/etc/elemental/release.yaml")
check("release-recorded", bool(marker and "releaseManifest:" in marker),
      "/etc/elemental/release.yaml: " + (" ".join(l for l in marker.splitlines() if ":" in l and not l.startswith("#")) if marker else "ABSENT"))
if SPLIT is not None:
    left = sorted(os.path.relpath(os.path.join(d, f), SPLIT) for d, _, fs in os.walk(SPLIT) for f in fs)
    check("split-directory", True, f"files customize kept out of the image in split mode: {left or 'none (the directory is empty)'}")

say()
say("Not looked at: binary content (kernel, initrd, RKE2 archive, elemental3ctl) is listed and hashed, not opened.")
say("Hashes of every file on the recovery partition: recovery/sha256.txt in the extraction directory.")
passed = all(c["passed"] for c in checks)
say(f"RESULT: {'PASS' if passed else 'FAIL'} ({sum(c['passed'] for c in checks)}/{len(checks)} checks)")
open(REPORT, "w").write("\n".join(out) + "\n")
json.dump({"passed": passed, "checks": checks}, open(RESULT, "w"), indent=2)
sys.exit(0 if passed else 1)
