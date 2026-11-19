#!/usr/bin/env python3
"""Génère les .torrent de test et affiche les hashes attendus.

Usage (depuis internal/torrent/testdata) : python3 gen.py

Les hashes sont calculés ici, indépendamment du code Go, avec hashlib, sur les
octets bruts du dict `info` tels qu'écrits dans le fichier. Les constantes de
torrent_test.go doivent correspondre à la sortie de ce script.
Aucun accès réseau ; les fichiers sont déterministes.
"""
import hashlib
import os


def benc(v):
    if isinstance(v, int):
        return b"i%de" % v
    if isinstance(v, bytes):
        return b"%d:%s" % (len(v), v)
    if isinstance(v, str):
        return benc(v.encode())
    if isinstance(v, list):
        return b"l" + b"".join(benc(x) for x in v) + b"e"
    if isinstance(v, dict):
        return b"d" + b"".join(benc(k) + benc(v[k]) for k in sorted(v)) + b"e"
    raise TypeError(v)


def torrent(info_raw, extra):
    """Assemble le fichier : les octets bruts de info sont insérés tels quels."""
    d = dict(extra)
    parts = [(b"info", info_raw)]
    body = b"".join(benc(k) + v for k, v in sorted([(k.encode(), benc(v)) for k, v in d.items()] + parts))
    return b"d" + body + b"e"


PIECES = bytes(range(20)) * 2  # 2 pièces SHA-1 factices (40 octets)
out = {}

# v1 mono-fichier, announce seul
info1 = benc({"length": 32768, "name": "mono.bin", "piece length": 16384, "pieces": PIECES})
out["v1.torrent"] = (info1, torrent(info1, {"announce": "http://tracker.example/announce", "comment": "v1"}))

# v1 multi-fichiers avec padding file + announce-list en 3 tiers (doublon et schémas à filtrer)
info_multi = benc({
    "files": [
        {"length": 10000, "path": ["dir", "a.bin"]},
        {"attr": "p", "length": 6384, "path": [".pad", "6384"]},
        {"length": 16384, "path": ["b.bin"]},
    ],
    "name": "multi",
    "piece length": 16384,
    "pieces": PIECES,
    "private": 1,
})
out["multi.torrent"] = (info_multi, torrent(info_multi, {
    "announce": "http://t1.example/announce",
    "announce-list": [
        ["http://t1.example/announce", "udp://t2.example:6969/announce"],
        ["ftp://ignored.example/x", "http://t1.example/announce"],
        ["https://t3.example/announce"],
        ["wss://ignored.example/"],
    ],
}))

# v1 avec clés de info NON TRIÉES : le hash doit porter sur les octets bruts.
info_unsorted = (b"d" + benc("pieces") + benc(PIECES) + benc("name") + benc("unsorted.bin")
                 + benc("length") + benc(100) + benc("piece length") + benc(16384) + b"e")
out["unsorted.torrent"] = (info_unsorted, torrent(info_unsorted, {"announce": "udp://t.example:80/announce"}))

# Hybride (BEP 52) : pieces + file tree + meta version 2
ROOT = hashlib.sha256(b"root").digest()
info_hybrid = benc({
    "file tree": {"hybrid.bin": {"": {"length": 32768, "pieces root": ROOT}}},
    "length": 32768,
    "meta version": 2,
    "name": "hybrid.bin",
    "piece length": 16384,
    "pieces": PIECES,
})
out["hybrid.torrent"] = (info_hybrid, torrent(info_hybrid, {"announce": "http://h.example/announce"}))

# v2 pur : pas de `pieces`, taille = somme du file tree
info_v2 = benc({
    "file tree": {
        "d": {"x.bin": {"": {"length": 20000, "pieces root": ROOT}}},
        "y.bin": {"": {"length": 5000, "pieces root": ROOT}},
    },
    "meta version": 2,
    "name": "v2dir",
    "piece length": 16384,
})
out["v2.torrent"] = (info_v2, torrent(info_v2, {"announce": "http://v2.example/announce"}))

here = os.path.dirname(os.path.abspath(__file__))
for name, (info, data) in out.items():
    with open(os.path.join(here, name), "wb") as f:
        f.write(data)
    assert info in data  # octets bruts présents tels quels
    print("%-18s sha1=%s sha256=%s" % (name, hashlib.sha1(info).hexdigest(), hashlib.sha256(info).hexdigest()))
