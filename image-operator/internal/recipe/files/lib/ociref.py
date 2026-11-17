#!/usr/bin/python3
"""Read facts about an OCI image straight from its registry (no container runtime needed).

  ociref.py digest <ref>     print the digest the tag (or digest) resolves to

<ref> is registry/repo:tag or registry/repo@sha256:..., with or without oci://. Plain HTTP is used
for a registry named in OCIREF_INSECURE (comma separated host:port list). Anonymous access only,
plus the credentials of REGISTRY_AUTH_FILE (podman/docker auth.json) when that variable is set.
Standard library only.
"""
import hashlib, json, os, re, sys, urllib.error, urllib.parse, urllib.request

ACCEPT = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])


def parse(ref):
    ref = re.sub(r"^(oci|docker)://", "", ref)
    host, _, rest = ref.partition("/")
    if not rest:
        sys.exit(f"ociref: not a full reference: {ref}")
    if "@" in rest:
        repo, _, tag = rest.partition("@")
    elif ":" in rest:
        repo, _, tag = rest.rpartition(":")
    else:
        repo, tag = rest, "latest"
    return host, repo, tag


def basic_auth(host):
    path = os.environ.get("REGISTRY_AUTH_FILE")
    if not path:
        return None
    with open(path) as f:
        entry = json.load(f).get("auths", {}).get(host)
    return entry.get("auth") if entry else None


class Registry:
    def __init__(self, host, repo):
        insecure = os.environ.get("OCIREF_INSECURE", "").split(",")
        self.base = f"{'http' if host in insecure else 'https'}://{host}/v2/{repo}"
        self.host, self.repo, self.token = host, repo, None

    def _open(self, url, method="GET"):
        req = urllib.request.Request(url, method=method, headers={"Accept": ACCEPT})
        if self.token:
            req.add_header("Authorization", "Bearer " + self.token)
        return urllib.request.urlopen(req, timeout=120)

    def open(self, path, method="GET"):
        url = f"{self.base}/{path}"
        try:
            return self._open(url, method)
        except urllib.error.HTTPError as e:
            if e.code != 401 or self.token:
                raise
            ch = dict(re.findall(r'(\w+)="([^"]*)"', e.headers.get("WWW-Authenticate", "")))
            if "realm" not in ch:
                raise
            q = ch["realm"] + "?" + urllib.parse.urlencode(
                {"service": ch.get("service", ""), "scope": f"repository:{self.repo}:pull"})
            treq = urllib.request.Request(q)
            auth = basic_auth(self.host)
            if auth:
                treq.add_header("Authorization", "Basic " + auth)
            with urllib.request.urlopen(treq, timeout=60) as r:
                body = json.load(r)
            self.token = body.get("token") or body.get("access_token")
            return self._open(url, method)

    def manifest(self, tag):
        with self.open(f"manifests/{tag}") as r:
            body = r.read()
        return "sha256:" + hashlib.sha256(body).hexdigest(), json.loads(body)


def main():
    args = sys.argv[1:]
    if len(args) != 2 or args[0] != "digest":
        sys.exit(__doc__)
    host, repo, tag = parse(args[1])
    reg = Registry(host, repo)
    digest, man = reg.manifest(tag)
    if tag.startswith("sha256:") and tag != digest:
        sys.exit(f"ociref: registry returned {digest} for {tag}")
    print(digest)


if __name__ == "__main__":
    main()
