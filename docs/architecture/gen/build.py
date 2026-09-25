#!/usr/bin/env python3
"""Regenerate every architecture diagram:  python3 docs/architecture/gen/build.py"""
import importlib, os, sys, glob
here = os.path.dirname(os.path.abspath(__file__))
os.chdir(here); sys.path.insert(0, here)
os.makedirs("../svg", exist_ok=True)
built = []
for f in sorted(glob.glob("d*_*.py")):
    mod = importlib.import_module(f[:-3])
    name = f[1:3] + "-" + f[4:-3].replace("_", "-") + ".svg"
    out = os.path.join("..", "svg", name)
    mod.build().save(out)
    built.append(name)
    print(f"  built {name}")
print(f"{len(built)} diagrams")
