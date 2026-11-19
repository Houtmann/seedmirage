# seedmirage

Simulateur de seed BitTorrent **local**, écrit en Go. Il annonce aux trackers un upload réaliste
sans transférer de données, en se faisant passer pour un client BitTorrent récent (qBittorrent, Transmission,
Deluge, rTorrent, BiglyBT…). Interface web locale, binaire unique ou Docker.

Idée inspirée de [JOAL](https://github.com/anthonyraymond/joal), écrit de zéro : aucun code, fichier ni format n'en est repris.

> ⚠️ La plupart des trackers privés interdisent ce type d'outil et bannissent les comptes détectés.
> Vous êtes seul responsable de son usage.

## État

Version de développement : voir `docs/` pour les profils, le mode capture et Docker.

## Lancement (cible)

```bash
seedmirage serve --data-dir ./data      # UI sur http://127.0.0.1:7070
```

```bash
mkdir -p data && docker compose up -d --build   # voir docs/docker.md
```

## Licence

MIT : voir [`LICENSE`](LICENSE). Attributions des composants embarqués : [`THIRD_PARTY.md`](THIRD_PARTY.md).
