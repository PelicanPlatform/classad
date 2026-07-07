#!/usr/bin/env python3
"""Generate the collections/vm benchmark corpus from a condor_status dump.

Samples ClassAds from a `condor_status -any -l` dump *proportionally by ad type*
(MyType) so the corpus mirrors the real mix of Machine / StartD / DaemonMaster /
etc., with a small floor so every rare daemon type is still represented. The
result is gzip-compressed to a target size and written to the output path.

The multi-GB source dump is not committed; only the small gzipped corpus is.
Invoked by `make corpus` (see the repository Makefile).

Usage:
    gen_corpus.py <input-dump.ads> <output.ads.gz> [target_gz_bytes]
"""
import gzip
import re
import sys

FLOOR = 2  # keep at least this many ads of each type when available

MYTYPE = re.compile(rb'^MyType = "([^"]*)"', re.M)


def load_blocks(path):
    with open(path, "rb") as f:
        data = f.read()
    by_type = {}
    for block in re.split(rb'\n[ \t]*\n', data):
        block = block.strip(b'\n')
        if not block:
            continue
        m = MYTYPE.search(block)
        t = m.group(1).decode() if m else "?"
        by_type.setdefault(t, []).append(block)
    return by_type


def sample(by_type, frac):
    """Pick ceil-ish frac of each type (>= FLOOR when available), spread evenly."""
    picked = []
    for lst in by_type.values():
        k = max(min(len(lst), FLOOR), round(len(lst) * frac))
        k = min(k, len(lst))
        for j in range(k):
            picked.append(lst[int(j * len(lst) / k)])
    return picked


def gz_bytes(blocks):
    return gzip.compress(b'\n\n'.join(blocks) + b'\n', 9)


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    inp, outp = sys.argv[1], sys.argv[2]
    target = int(sys.argv[3]) if len(sys.argv) > 3 else 500_000

    by_type = load_blocks(inp)
    total = sum(len(v) for v in by_type.values())
    print(f"loaded {total} ads across {len(by_type)} types:")
    for t, v in sorted(by_type.items(), key=lambda kv: -len(kv[1])):
        print(f"  {t:<14} {len(v)}")

    # Binary-search the sampling fraction to hit ~target compressed bytes.
    lo, hi, best = 0.0, 0.05, None
    for _ in range(18):
        frac = (lo + hi) / 2
        blocks = sample(by_type, frac)
        gz = gz_bytes(blocks)
        best = (blocks, gz)
        if len(gz) < target:
            lo = frac
        else:
            hi = frac
        if abs(len(gz) - target) < target * 0.05:
            break

    blocks, gz = best
    with open(outp, "wb") as f:
        f.write(gz)

    counts = {}
    for b in blocks:
        m = MYTYPE.search(b)
        counts[m.group(1).decode() if m else "?"] = counts.get(
            m.group(1).decode() if m else "?", 0) + 1
    print(f"\nsampled {len(blocks)} ads -> {outp} ({len(gz)} gz bytes)")
    for t, c in sorted(counts.items(), key=lambda kv: -kv[1]):
        print(f"  {t:<14} {c}")


if __name__ == "__main__":
    main()
