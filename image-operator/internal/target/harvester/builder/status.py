# Writes /var/lib/e3-build/out/status.json from the environment run-build.sh sets. No input from the network.
import json, os, time

out = os.environ["OUT"]
name = os.environ["NAME"]
doc = {
    "key": os.environ["KEY"],
    "state": os.environ["STATE"],
    "step": os.environ["STEP"],
    "exitCode": int(os.environ.get("EXIT_CODE", "0") or 0),
    "error": os.environ.get("ERROR", ""),
    "startedAt": os.environ.get("STARTED", ""),
    "updatedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
}
if doc["state"] == "failed":
    # Where the build ran, for diagnosing permission problems: SELinux mode and the process context.
    try:
        with open("/sys/fs/selinux/enforce") as f:
            mode = "enforcing" if f.read().strip() == "1" else "permissive"
        with open("/proc/self/attr/current") as f:
            doc["selinux"] = mode + " " + f.read().strip("\x00\n ")
    except OSError:
        doc["selinux"] = "off"
    tail = ""
    for log in (name + ".build.log", "runner.log"):
        path = os.path.join(out, log)
        if os.path.exists(path) and os.path.getsize(path) > 0:
            with open(path, errors="replace") as f:
                tail = "".join(f.readlines()[-25:])[-3000:]
            break
    doc["logTail"] = tail
if doc["state"] == "done":
    with open(os.path.join(out, name + ".metadata.json")) as f:
        meta = json.load(f)
    with open(os.path.join(out, name + ".qcow2.sha512")) as f:
        doc["sha512"] = f.read().split()[0]
    doc.update({
        "file": meta["file"],
        "sha256": meta["sha256"],
        "sizeBytes": meta["sizeBytes"],
        "manifestDigest": meta["release"]["manifestDigest"],
        "kubernetesVersion": meta["kubernetes"]["version"],
        "buildSeconds": meta["buildSeconds"],
        "toolCommit": meta["elemental"]["commit"],
    })
tmp = os.path.join(out, ".status.json.tmp")
with open(tmp, "w") as f:
    json.dump(doc, f)
os.replace(tmp, os.path.join(out, "status.json"))
