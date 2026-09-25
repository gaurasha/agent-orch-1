#!/usr/bin/env python3
"""Check every link in the documentation.

  python3 docs/tools/check-links.py            # internal links + anchors (offline)
  python3 docs/tools/check-links.py --external # also fetch every external URL

Internal check: every relative link resolves to an existing file and, if it
carries a #fragment, to a heading in that file (GitHub slug rules).
External check: HEAD/GET each URL once (16 in parallel) and report anything
that is not 2xx/3xx. Example hosts (*.example) are skipped.
"""
import concurrent.futures, glob, os, re, sys, urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
DOCS = [ROOT + "/README.md", ROOT + "/DESIGN.md", ROOT + "/DEEP_DIVE.md", ROOT + "/TUTORIAL.md"] + \
       glob.glob(ROOT + "/docs/**/*.md", recursive=True)

def slug(h):
    h = re.sub(r"`", "", h.strip()).lower()
    h = re.sub(r"[^\w\- ]", "", h)
    return h.replace(" ", "-")

def anchors(path):
    out = set()
    for line in open(path, encoding="utf-8"):
        m = re.match(r"^(#{1,6})\s+(.*)$", line)
        if m:
            out.add(slug(m.group(2)))
    return out

def internal():
    bad = []
    for f in DOCS:
        d = os.path.dirname(f)
        for m in re.finditer(r"\]\(([^)\s]+)\)", open(f, encoding="utf-8").read()):
            t = m.group(1)
            if t.startswith(("http://", "https://", "mailto:")):
                continue
            path, _, anc = t.partition("#")
            target = os.path.normpath(os.path.join(d, path)) if path else f
            if not os.path.exists(target):
                bad.append((os.path.relpath(f, ROOT), t, "missing file"))
            elif anc and target.endswith(".md") and slug(anc) not in anchors(target) and anc not in anchors(target):
                bad.append((os.path.relpath(f, ROOT), t, "missing anchor"))
    return bad

def fetch(u):
    req = urllib.request.Request(u, headers={"User-Agent": "Mozilla/5.0 docs-link-check"})
    try:
        with urllib.request.urlopen(req, timeout=25) as r:
            return r.status, u
    except urllib.error.HTTPError as e:
        return e.code, u
    except Exception as e:  # noqa: BLE001
        return 0, f"{u}  ({type(e).__name__})"

def external():
    urls = set()
    for f in DOCS:
        for m in re.finditer(r"https?://[^\s)>\"`\]]+", open(f, encoding="utf-8").read()):
            u = m.group(0).rstrip(".,;:")
            if ".example" in u:
                continue
            urls.add(u)
    with concurrent.futures.ThreadPoolExecutor(16) as ex:
        results = list(ex.map(fetch, sorted(urls)))
    bad = [r for r in results if not (200 <= r[0] < 400)]
    return len(urls), bad

if __name__ == "__main__":
    bad = internal()
    print(f"internal: {len(DOCS)} files, {len(bad)} broken")
    for b in bad:
        print("  ", *b)
    rc = 1 if bad else 0
    if "--external" in sys.argv:
        n, ext = external()
        print(f"external: {n} URLs, {len(ext)} not reachable")
        for code, u in ext:
            print(f"  {code:3d} {u}")
        rc |= 1 if ext else 0
    sys.exit(rc)
