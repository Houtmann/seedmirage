# Profils de clients (format `schema` 1)

Un profil décrit comment un client BitTorrent annonce : peer_id, key, encodage, paramètres de query (ordre compris),
en-têtes et `numwant`. Le format est propre à seedmirage ; il est conçu pour reproduire **octet pour octet** la
requête du vrai client (invariant : requête identique, octet pour octet). Code : `internal/emulation`. Profils embarqués :
`internal/emulation/profiles/*.json`, copiés dans `clients/` au démarrage (voir « Profils embarqués »). Référence complète du format.

La vérité sur un client vient de son **code source** : chaque profil cite dans `references` les fichiers, tags et lignes
consultés, et dit dans `notes` ce qui reste incertain ou non reproduit.

## Clés

| Clé | Type | Rôle |
|---|---|---|
| `schema` | entier | Toujours `1`. |
| `name` | texte | `<family>-<version>`, doit correspondre au nom du fichier. |
| `family`, `version` | texte | Ex. `qbittorrent`, `5.3.0`. |
| `released` | `AAAA-MM-JJ` | Date de la version (au besoin celle du commit tagué, dit dans `notes`). |
| `source` | `manual` \| `capture` | Origine du profil. |
| `notes` | texte | Écarts, approximations, incertitudes. |
| `references` | liste `{url, note}` | Fichiers source consultés : lien permanent (tag ou commit, plage `#Lx-Ly`) et ce qu'il justifie. Obligatoire, `https://`. |
| `peerId` | objet | Voir ci-dessous. |
| `key` | objet, optionnel | Voir ci-dessous. Absent : le client n'envoie pas de key. |
| `encoding` | objet | `keep` : octets non encodés en plus de `[0-9A-Za-z]` (ASCII imprimable, sans `%`) ; `hex` : `upper` ou `lower`. Un paramètre de `query` peut porter son propre `encoding` (voir « query »). |
| `query` | liste ordonnée | Paramètres `{name, value, omit?}`. |
| `headers` | liste ordonnée | En-têtes `{name, value}` à valeur fixe, dans l'ordre d'émission. `Host` est ajouté par le client HTTP, en premier ; pour le placer ailleurs (BiglyBT/JDK), l'entrée `{"name": "Host", "value": "{host}"}` fixe sa position (seule valeur permise). |
| `udp` | objet, optionnel | `urlData` (booléen, vrai par défaut) : le client joint l'option BEP 41 « URLData » (chemin et query de l'URL du tracker) à ses annonces UDP. libtorrent (qBittorrent, Deluge) l'envoie, Transmission non : confirmé par capture. |
| `numwant` | objet | `none`, `started`, `completed`, `stopped` : les quatre clés sont obligatoires (entiers >= -1 ; -1 = défaut du tracker : le client n'envoie pas `numwant` en HTTP et envoie -1 en UDP, comme rTorrent). |

Les champs inconnus sont refusés (`Parse`) : un profil est un contrat.

### `peerId`

`prefix` (texte) ou `prefixHex` (hexadécimal, pour des octets non imprimables) + `suffix` + éventuel octet de somme.
`suffix` est un motif `regexgen` (sous-ensemble de la syntaxe `regexp/syntax`, octets 0x00-0xFF), de longueur **fixe**.
Le total fait exactement **20 octets** : `Validate` le vérifie sur 64 tirages, et chaque génération le revérifie.

`checksum` (optionnel) nomme une transformation appliquée au suffixe :

| Nom | Effet | Client |
|---|---|---|
| `mod36` | Ajoute un octet final tiré de `0-9a-z` tel que la somme des indices (dans `0-9a-z`) du suffixe et de cet octet soit nulle modulo 36 ; le suffixe doit être dans `0-9a-z`. | Transmission 4.1.4 (`tr_peerIdInit`, `session.cc`) |

### `key`

`pattern` : motif `regexgen` (ex. `[0-9A-F]{8}` = 32 bits en hexadécimal majuscule). Le client UDP (BEP 15) peut réutiliser `Announce.Key`.

### Portées de renouvellement (`renewal.scope`)

`renewal` accepte en plus, pour le **peer_id** seulement et la portée `torrent`, `ttlHours` (le peer_id est renouvelé à la première annonce qui suit son âge de `ttlHours` heures) et `ttlPublicOnly` (les torrents privés ne sont jamais renouvelés par l'âge). Transmission 3.00 : `{"scope": "torrent", "ttlHours": 6, "ttlPublicOnly": true}`.

| Portée | Comportement | Clients qui l'utilisent |
|---|---|---|
| `torrent` | Une valeur par torrent, stable tant que l'état du torrent est conservé : la vie du processus, ou jusqu'à éviction après `IdleTTL` (24 h par défaut) sans annonce. Un `started` répété ne la renouvelle pas (un client réel garde l'objet torrent en pause/reprise). | qBittorrent 4.6.7 et 5.3.0 (peer_id : `torrent.cpp` ; key : `tracker_key()`), Deluge 2.2.0, Transmission 4.1.4 et 3.00 (peer_id), rTorrent 0.16.28 (peer_id et key) |
| `process` | Une valeur pour tout le processus, partagée par les torrents. | Transmission 3.00 (key : `tr_announcer`) ; disponible pour les captures. |
| `session` | Une valeur par torrent, renouvelée à chaque annonce `started`. | Aucun client étudié à ce jour ; disponible pour les captures. |

Chaque valeur est tirée avec la source `*rand.Rand` injectée ; l'état est évincé via l'horloge `clock.Clock` injectée.

### `query`

Chaque paramètre est émis dans l'ordre de la liste, sous la forme `name=value`, séparés par `&`. Un paramètre peut avoir son propre `encoding` (même forme que celui du profil) qui remplace celui du profil pour ses variables : Transmission 3.00 encode l'info_hash en hexadécimal minuscule et `ipv6` en majuscule. Le résultat
(`Announce.RawQuery`, sans `?`) n'est jamais « nettoyé » : un paramètre soit est émis, soit est omis selon des règles explicites.

`value` est un gabarit : texte littéral (sans accolades) et variables `{nom}`, encodées selon `encoding`. `{nom:raw}` insère
la valeur telle quelle (le client réel ne l'encode pas ; seul usage actuel : `trackerid` et `ip` de Transmission).
`{info_hash:raw}` est interdit.

| Variable | Valeur | Omission exigée |
|---|---|---|
| `{info_hash}` | 20 octets, encodés octet par octet | |
| `{peer_id}` | 20 octets du peer_id | |
| `{port}`, `{uploaded}`, `{downloaded}`, `{left}`, `{numwant}` | décimal | |
| `{event}` | `started`, `stopped` ou `completed` | `no-event` |
| `{key}` | key du torrent | `no-key` si le profil n'a pas de section `key` |
| `{ip}` | `PublicIP` | `no-ip` |
| `{ipv4}` / `{ipv6}` | `PublicIP` si IPv4 / IPv6 (jamais IPv4 mappée en IPv6) | `no-ipv4` / `no-ipv6` |
| `{tracker_id}` | `trackerid` reçu du tracker | `no-tracker-id` |

`Validate` refuse un paramètre qui utilise une variable sans l'omission correspondante.

Règles `omit` (le paramètre est supprimé si **l'une** des conditions est vraie) :

| Règle | Vraie quand |
|---|---|
| `no-event` | annonce régulière (aucun événement) |
| `no-ip` | `PublicIP` inconnue |
| `no-ipv4` / `no-ipv6` | `PublicIP` absente ou d'une autre famille |
| `no-key` | le profil n'a pas de `key` |
| `no-tracker-id` | aucun trackerid connu |
| `public-torrent` | le torrent n'a pas le drapeau `private` |

### `headers`

Valeurs fixes, ordre du profil. Le client HTTP écrit la requête à la main et place `Host` en premier, sauf entrée `Host` `{host}` dans `headers` : il l'écrit alors à cette position, avec l'hôte de l'URL d'annonce. Il doit décoder
les codages annoncés dans `Accept-Encoding` (profil Transmission : `deflate, gzip`).

Écart connu (v1) : Transmission envoie une annonce IPv4 puis une IPv6 quand l'hôte a les deux ; seedmirage n'envoie
**qu'une annonce par tracker**, sur l'adresse que résout le système. Le tracker ne voit donc pas la seconde annonce.
Le client HTTP (`internal/tracker`) n'ajoute aucun en-tête (ni `Accept-Encoding` ni `User-Agent` Go), joint la query
à celle de l'URL d'annonce par `&` (sinon `?`) et omet le port de `Host` s'il est celui du schéma.

## Profils embarqués

Triés par famille puis par version sémantique (`CompareVersions` : `4.10.0` après `4.9.0`, `3.00` = `3.0`). `emulation.LatestEmbedded("qbittorrent")` donne le profil par défaut de la config (`config.Defaults`).
Chaque profil cite le code source du client (tag, fichier, lignes) dans `references` ; ce qui n'a pas pu être établi est dit dans `notes`.

| Famille | Version | Date de sortie | Code source cité |
|---|---|---|---|
| qbittorrent | 5.3.0 | 2026-10-19 | qBittorrent `release-5.3.0` + libtorrent `v2.0.15` |
| qbittorrent | 4.6.7 | 2024-09-16 | qBittorrent `release-4.6.7` + libtorrent `v1.2.20` (annonce identique à 2.0.15) |
| transmission | 4.1.4 | 2026-10-22 | Transmission `4.1.4` |
| transmission | 3.00 | 2020-05-22 | Transmission `3.00` |
| deluge | 2.2.0 | 2025-04-28 | Deluge `deluge-2.2.0` + libtorrent `v2.0.15` (User-Agent : `lt.__version__` = 2.0.15.0 supposé) |
| rtorrent | 0.16.28 | 2026-11-11 | libtorrent (rakshasa) `v0.16.28` + rTorrent `v0.16.28` |

### Vérification par capture

Les 6 profils ont été comparés, le 2026-11-19, à des **vrais clients** lancés dans Docker (`deploy/capture/docker-compose.yml`,
pilotés par `scripts/capture/<client>.sh`, procédure de [`capture.md`](capture.md) : `started`, annonce régulière, pause/reprise
d'un torrent, redémarrage du client, `stopped`, sur 4 torrents HTTP dont un privé et 1 torrent UDP). Les captures réduites aux
annonces et anonymisées (IP publique remplacée par `192.0.2.10`) sont dans `internal/emulation/testdata/captures/<profil>/` ;
`TestCapturedFixtures` (`internal/capture/fixtures_test.go`) les rejoue sans réseau contre le profil embarqué avec `capture.Diff`
(HTTP) et `capture.DiffUDP` (UDP BEP 15). `known-diffs.txt` liste, par capture, les seuls écarts tolérés et leur raison.

| Profil | Client capturé (image Docker) | Résultat |
|---|---|---|
| qbittorrent-5.3.0 | qBittorrent 5.3.0, libtorrent 2.1.2 (`lscr.io/linuxserver/qbittorrent:5.3.0`) | identique, HTTP et UDP |
| qbittorrent-4.6.7 | qBittorrent 4.6.7, libtorrent 1.2.19 (`lscr.io/linuxserver/qbittorrent:4.6.7-libtorrentv1`) | identique, HTTP et UDP |
| transmission-4.1.4 | Transmission 4.1.4 (`lscr.io/linuxserver/transmission:4.1.4`) | **corrigé** : `ip` retiré (seulement `ipv4`/`ipv6`), UDP sans option BEP 41, `num_want` UDP 0 pour `stopped` ; seul écart : `Accept-Encoding` |
| transmission-3.00 | Transmission 3.00 (`lscr.io/linuxserver/transmission:version-3.00-r8`) | identique (UDP sans option BEP 41) ; seul écart : `Accept-Encoding` |
| deluge-2.2.0 | Deluge 2.2.0 (`lscr.io/linuxserver/deluge:2.2.0`) | identique ; seul écart : version de libtorrent dans le `User-Agent` (2.0.13.0 dans l'image) |
| rtorrent-0.16.28 | rTorrent **0.16.25** (`crazymax/rtorrent-rutorrent:5.3.18-0.16.25`, pas d'image 0.16.28) | identique aux versions près (`User-Agent`, préfixe de peer_id `-lt1019-`, justifiés par le code source) ; UDP sans option BEP 41 |

Écart commun à Transmission et rTorrent : la valeur de `Accept-Encoding` vient de la libcurl compilée dans le client
(`deflate, gzip, br, zstd` avec celle des images Alpine). Les profils gardent `deflate, gzip` parce que le client HTTP de
seedmirage ne décode ni `br` ni `zstd` ; un client réel sur une autre plate-forme (binaires officiels macOS/Windows) peut
annoncer autre chose, ce que seule une capture de ce client permettrait de trancher.

`udp.urlData` (nouveau, voir « Clés ») : libtorrent (qBittorrent, Deluge) envoie l'option BEP 41 `URLData` dans ses annonces UDP,
Transmission et rTorrent non.

**Scrape** (observé, aucune modification du moteur ici) : les clients capturés scrapent en HTTP en encodant
l'`info_hash` **comme leur annonce** (qBittorrent et Deluge en hexadécimal minuscule, Transmission 4.1.4 et rTorrent en majuscule,
Transmission 3.00 en minuscule) ; Transmission 4.1.4 envoie plusieurs paramètres `info_hash` dans une seule requête `/scrape`.

### Familles sans profil

- **µTorrent / BitTorrent** : clients Windows (et macOS) fermés, sans image Linux ; aucun code source à lire. Un profil exige
  une **capture humaine** avec `seedmirage capture --listen <adresse LAN>` (voir « Client sur l'hôte » dans
  [`capture.md`](capture.md)). Non fourni.
- **BiglyBT** : non fourni, faute de pouvoir le rendre fidèle à l'octet près. Le format sait désormais placer `Host` (entrée
  `Host` `{host}` de `headers`, respectée par `internal/tracker` et `capture`), mais la requête réelle dépend (a) du JDK embarqué
  par l'installeur (`HttpURLConnection` : ordre `User-Agent`, `Connection`, `Accept-Encoding`, `Host`, `Accept`, valeur par
  défaut d'`Accept` variable selon la version), (b) d'un filtre HTTP local de BiglyBT (`ClientIDManagerImpl`, v4.2.0.0) qui, quand
  il est actif, réécrit `Host` en `hôte:port` explicite même pour le port 80, et (c) du suffixe optionnel du `User-Agent`
  (`;système;Java x`). Aucune image Docker fiable (les images communautaires embarquent un VNC ou un VPN) et pas de capture :
  un profil `manual` aurait une confiance trop faible pour l'invariant 5. À faire : capture d'un BiglyBT 4.2.0.0 réel, puis profil
  avec `Host` placé d'après la capture. La dernière version publiée est BiglyBT 4.2.0.0 (2026-11-19).

### Installation dans `clients/`

Au démarrage (`datadir.Ensure`), chaque profil embarqué est copié dans `clients/<nom>.json` :

- absent : copié ;
- présent et identique à l'embarqué : rien à faire ;
- présent, identique à ce qui y avait été installé (hash SHA-256 dans `clients/.embedded.json`) mais différent de l'embarqué actuel : mis à jour avec le binaire ;
- présent et modifié par l'utilisateur (ou fichier qu'on n'a pas installé) : **jamais écrasé**.

Les écritures sont atomiques (fichier temporaire puis rename). Un profil retiré des embarqués quitte le manifeste mais son fichier reste.

## API Go

```go
p, _ := emulation.LoadEmbedded("qbittorrent-5.3.0") // ou emulation.Parse(data)
e, _ := emulation.New(p, clk, rng, emulation.Options{})
a, _ := e.BuildAnnounce(emulation.AnnounceParams{
    InfoHash: ih, Port: 51413, Uploaded: u, Downloaded: d, Left: 0,
    Event: emulation.EventStarted, PublicIP: ip, Private: priv, TrackerID: tid,
})
// a.RawQuery, a.Headers, a.PeerID, a.Key, a.Numwant
```

`Emulator` est sûr en concurrence. `Forget(infoHash)` oublie un torrent supprimé ou archivé.

## Exemple commenté (extrait de `transmission-4.1.4.json`)

```jsonc
{
  "schema": 1,
  "name": "transmission-4.1.4",
  "peerId": {
    "prefix": "-TR4140-",              // -TR + base62(4,1,4) + 0 (stable) + -
    "suffix": "[0-9a-z]{11}",          // 11 caractères aléatoires...
    "checksum": "mod36",               // ...+ 1 caractère de somme = 20 octets
    "renewal": {"scope": "torrent"}
  },
  "key": {"pattern": "[0-9A-F]{8}", "renewal": {"scope": "torrent"}},
  "encoding": {"keep": "-_.~", "hex": "upper"},
  "query": [
    {"name": "info_hash", "value": "{info_hash}"},
    {"name": "numwant",   "value": "{numwant}"},
    {"name": "compact",   "value": "1"},                              // valeur fixe
    {"name": "event",     "value": "{event}", "omit": ["no-event"]}, // absent d'une annonce régulière
    {"name": "ipv4",      "value": "{ipv4}", "omit": ["no-ipv4"]}   // absent si PublicIP inconnue ou IPv6
  ],
  "headers": [{"name": "User-Agent", "value": "Transmission/4.1.4"}],
  "numwant": {"none": 80, "started": 80, "completed": 80, "stopped": 0}
}
```

## Linter (`seedmirage check-profile`)

```bash
seedmirage check-profile internal/emulation/profiles      # un dossier (*.json) ou des fichiers
```

Affiche un constat par ligne (`<fichier>: <niveau> <code>: <message>`) ou `ok`. Code de sortie **1** s'il y a au moins une erreur, 0 sinon (les avertissements ne font pas échouer). Code : `emulation.Lint` / `emulation.LintData`.

| Code | Niveau | Règle |
|---|---|---|
| `parse` | error | JSON illisible ou champ inconnu. |
| `invalid` | error | Échec de `Validate` (règles de ce document). |
| `host-header` | error | En-tête `Host` autre que l'entrée de position `{host}`. |
| `duplicate-param` | error | Paramètre de query en double. |
| `ua-version` | error | Le `User-Agent` ne contient pas la `version` du profil. |
| `peerid-prefix` | error | Préfixe de peer_id incohérent avec la version (qBittorrent `-qB5300-`, Deluge `-DE220s-`, Transmission `-TR4140-` ; rTorrent n'est pas vérifié : son préfixe est celui de libtorrent ; Transmission 4.x au-delà d'un chiffre par composante : règle sautée). |
| `no-user-agent` | warn | Aucun `User-Agent`. |
| `numwant-atypical` | warn | `numwant` différent de la valeur habituelle de la famille (200 qBittorrent/Deluge, 80 Transmission, -1 rTorrent ; 0 à l'arrêt sauf rTorrent). |
| `accept-encoding` | warn | `Accept-Encoding` absent, ou différent de celui de la famille, ou annoncé alors que la famille n'en envoie pas. |
| `accept-decode` | warn | Codage annoncé que le client HTTP ne décode pas (autre que `gzip`, `deflate`, `identity`). |
| `outdated` | warn | Au moins `MaxVersionsBehind` (2) profils embarqués plus récents dans la même famille. |

Les familles inconnues (captures de clients non couverts) ne subissent que les règles génériques.

## Versions obsolètes (`seedmirage profiles outdated --online`)

Interroge les API publiques de versions (GitHub : `qbittorrent/qBittorrent`, `transmission/transmission`, `BiglySoftware/BiglyBT`, `rakshasa/rtorrent` ; tags `deluge-X.Y.Z` de `deluge-torrent/deluge`) et liste les familles dont la dernière version stable est plus récente que le profil embarqué le plus récent (ou n'a aucun profil). Accès réseau **uniquement** avec `--online` ; pas de jeton, `User-Agent: seedmirage-release-check`, résultats mis en cache 1 h dans le processus. Code : `internal/releases` (`New(Config{...}).Check(ctx, embedded)`, URL de base et `http.Client` injectables).

## Ajouter un profil

1. **Capturer** : `seedmirage capture` sur le vrai client, ou lire son code source (tag précis) et noter dans `references` chaque fichier et plage de lignes consultés.
2. **Écrire** `internal/emulation/profiles/<famille>-<version>.json` (`source`: `capture` ou `manual`, écarts dans `notes`).
3. **Linter** : `seedmirage check-profile internal/emulation/profiles` doit être sans erreur ; justifier chaque avertissement dans `notes`.
4. **Golden** : `go test ./internal/emulation/ -update`, relire les fichiers de `testdata/golden/` contre le code source cité, puis `go test ./internal/emulation/...`.
5. **Revue** : `make check`, puis pull request (un profil par PR, tableau « Profils embarqués » de ce document à jour).

## Tests golden

`internal/emulation/testdata/golden/<profil>-<événement>.txt` contient la query, les en-têtes, le peer_id (hex), la key et
le `numwant` pour `started`, `regular`, `completed`, `stopped` (graine et horloge fixes), plus `started-ip` (adresse,
trackerid, torrent privé). Régénération : `go test ./internal/emulation/ -update`, puis relecture contre le code source
cité dans `references`.
