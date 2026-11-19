# Mode capture : fabriquer un profil à partir du vrai client

`seedmirage capture` lance un tracker local (HTTP **et** UDP, même port) qui enregistre ce qu'envoie un vrai client
BitTorrent, puis en déduit un profil au format [`schema: 1`](profiles.md) (`source: capture`). C'est la façon de suivre une
nouvelle version d'un client sans attendre : la vérité vient de ce que le client envoie réellement.

Code : `internal/capture` (enregistrement, inférence, `Diff`) et `cmd/seedmirage/capture.go`.

## Procédure pas à pas

1. **Lancer le tracker** (loopback par défaut) :

   ```bash
   seedmirage capture --listen 127.0.0.1:6969 --out ./capture --torrents 4
   ```

   Il écrit dans `./capture` : les `.torrent` factices (`capture-NN-http.torrent`, `capture-NN-udp.torrent`),
   `manifest.json` (torrents générés, dont le drapeau `private`), puis, au fil de l'eau, `raw.jsonl`. Un dossier déjà utilisé
   est refusé : rien n'est écrasé.
   Les torrents n'ont aucune donnée (taille 90 Mio à 4 Gio, tailles variées) : le client annonce `left > 0`, `downloaded = 0`.
   Les `info_hash` sont choisis parmi 20 000 candidats pour contenir les octets difficiles à encoder (0x00, 0x20, 0x80, 0xFF,
   `- _ . ~ ! * ( ) ' % +`) : c'est ce qui permet de déduire quels octets le client laisse bruts et lesquels il encode.
   Le dernier torrent HTTP est privé (certains clients n'envoient `ipv4`/`ipv6` que pour ceux-là).
   `--udp-torrents` (défaut 1) ajoute des torrents annoncés **seulement** en UDP : un client n'annonce en général qu'à un
   tracker par palier, il faut donc des torrents distincts pour voir les deux protocoles.

2. **Dans le vrai client**, dans cet ordre :
   1. ajouter tous les `.torrent`, sans télécharger de fichier ni lancer de vérification ; laisser passer au moins deux
      annonces par torrent (l'intervalle renvoyé est de 60 s, les pairs sont vides) ;
   2. mettre **un seul** torrent en pause, attendre 10 s, le reprendre : sans cette étape, les portées de renouvellement
      `torrent` et `session` ne peuvent pas être distinguées ;
   3. quitter le client, attendre 10 s, le relancer : tous les torrents redémarrent ensemble (distingue la portée `process`
      et vérifie ce qui survit à un redémarrage) ; attendre une annonce régulière ;
   4. retirer les torrents du client (annonce `stopped`).

3. **Ctrl-C** dans `seedmirage capture` : le tracker s'arrête, puis `capture/<famille>-<version>.json` et
   `capture/report.md` sont écrits.

4. **Relire `report.md`**. Il liste d'abord les **incertitudes** (confiance `low`, puis `medium`), puis les déductions sûres.
   Typiquement : alphabet du suffixe partiellement observé, octets d'`info_hash` jamais vus, événement `completed` non observé
   (la valeur de `numwant` est alors recopiée), portée `session` non exclue. Corriger le profil à la main si besoin, ajouter
   les références au code source (`references`, `--reference`), compléter `notes`.

5. **Valider** : `seedmirage check-profile capture/<profil>.json` (sans erreur), puis la suite de
   [« Ajouter un profil »](profiles.md#ajouter-un-profil) (golden, revue).

`seedmirage capture --infer-only --out ./capture` relance l'analyse d'un `raw.jsonl` existant (par exemple reçu d'un autre
poste) sans démarrer de tracker ; les fichiers dérivés sont réécrits.

## Client Docker

`deploy/capture/docker-compose.yml` fournit qBittorrent, Transmission, Deluge et rTorrent (un profil compose par client,
versions épinglées). Le tracker tourne sur l'hôte et les conteneurs le joignent par `host.docker.internal` :

```bash
seedmirage capture --listen 172.17.0.1:6969 --announce-host host.docker.internal:6969 --out ./capture
docker compose -f deploy/capture/docker-compose.yml --profile qbittorrent up -d
```

`172.17.0.1` est l'adresse habituelle du pont Docker ; `--listen 0.0.0.0:6969` convient aussi mais expose le tracker sur
toutes les interfaces. Toute adresse hors loopback affiche un **avertissement** : n'importe quelle machine qui l'atteint peut
envoyer des annonces (les requêtes brutes sont enregistrées dans le dossier de sortie). Arrêter la capture ensuite.

Les tags du fichier compose ont été vérifiés le 2026-11-19 (voir « Captures réalisées » dans [`profiles.md`](profiles.md)) :
qBittorrent 5.3.0, Transmission 4.1.4 (et 3.00 : `TRANSMISSION_TAG=version-3.00-r8`), Deluge 2.2.0, rTorrent 0.16.25
(image communautaire `crazymax/rtorrent-rutorrent`, pas de 0.16.28 publiée). Contrôler la version réellement lancée (journal
du conteneur, interface web) et que le profil produit porte bien cette version. Sur Docker Desktop/OrbStack pour macOS,
`--listen 127.0.0.1:6969 --announce-host host.docker.internal:6969` suffit (TCP et UDP) : pas besoin d'exposer le tracker.

### Scripts de pilotage

`scripts/capture/<client>.sh <dossier de capture>` (qbittorrent, transmission, deluge, rtorrent) déroule la procédure
ci-dessus par l'API locale du client (WebUI, RPC, JSON-RPC) : ajout, annonce régulière, pause/reprise d'un torrent,
arrêt/relance du conteneur, retrait. Variable `INTERVAL` : attente d'une annonce régulière (60 s par défaut ; **300 s**
pour qBittorrent et Deluge : libtorrent borne l'intervalle d'annonce à 5 minutes, l'intervalle de 60 s du tracker de capture
est ignoré). Plusieurs clients peuvent être capturés en parallèle avec des trackers sur des ports différents.
`go run ./scripts/capture/reduce <capture> <sortie>` réduit une capture à ses annonces et remplace les adresses IP annoncées
par des adresses de documentation (192.0.2.10) : c'est ainsi que sont produites les fixtures de
`internal/emulation/testdata/captures/<profil>/`, rejouées sans réseau par `TestCapturedFixtures`.

## Client sur l'hôte (µTorrent sous Windows, etc.)

1. Lancer `seedmirage capture --listen <adresse-LAN-de-l'hôte>:6969 --announce-host <cette-adresse>:6969 --out capture`
   (ou, si le client tourne sur la même machine, garder `127.0.0.1` : certains clients refusent un tracker en loopback ;
   dans ce cas utiliser l'adresse LAN et accepter l'avertissement).
2. Ajouter les `.torrent` de `capture/` au client et suivre les étapes ci-dessus.
3. Désactiver dans le client ce qui parasite : DHT, PEX, découverte locale, limite « au moins un tracker » ; un client Windows
   peut demander d'autoriser le pare-feu pour le port 6969.
4. Pour un client qui n'est pas sur la même machine : copier `capture/` après Ctrl-C et relancer l'analyse avec `--infer-only`
   si besoin.

## Ce qui est enregistré (`raw.jsonl`)

Une ligne JSON par requête :

| Champ | Contenu |
|---|---|
| `time`, `transport` (`http`/`udp`), `remote` | Horodatage (horloge injectée), protocole, adresse du client. |
| `head` (HTTP) | Ligne de requête et en-têtes **bruts**, séparés par CRLF, en base64 : c'est la référence exacte (ordre et casse d'origine, octets hors UTF-8 compris). |
| `lines` (HTTP) | Même contenu en clair, pour la lecture humaine (peut perdre les octets non UTF-8 ; ne sert pas à l'analyse). |
| `packet` (UDP) | Datagramme reçu, en hexadécimal. |

La requête HTTP est lue sur la connexion TCP par le tracker factice de `internal/trackertest`, jamais par `net/http` : aucune
normalisation (casse des en-têtes, hexadécimal du `%XX`, ordre). `info_hash`, `peer_id` et `key` sont décodés octet par
octet (`PercentDecode`), jamais convertis en texte.

## Ce qui est déduit

| Élément | Déduction | Confiance |
|---|---|---|
| Ordre des paramètres | séquence commune aux requêtes ; paramètres absents insérés après leur prédécesseur | high ; low si l'ordre diffère entre requêtes |
| Valeurs fixes (`compact=1`, `supportcrypto=1`…) | valeur identique dans toutes les requêtes | high |
| Variables (`{info_hash}`, `{peer_id}`, `{port}`, `{uploaded}`, `{event}`, `{numwant}`, `{key}`, `{ip}`…) | reconnues par le nom du paramètre | high |
| Règles d'omission | `no-event` si le paramètre manque exactement aux annonces régulières ; `public-torrent` si absent des torrents publics (d'après `manifest.json`) ; `no-ip`, `no-ipv4`, `no-ipv6`, `no-tracker-id` exigés par le format | medium |
| `encoding` | octets non alphanumériques vus bruts → `keep` ; casse du `%XX` → `hex` ; encodage propre à un paramètre (ex. `info_hash` en hexadécimal minuscule, `peer_id` en majuscule) → `encoding` du paramètre | high à low selon le nombre de valeurs ; les octets jamais observés sont listés (ils sont encodés par défaut) |
| `peerId` | préfixe = empreinte `-XXnnnn-` (8 octets) ou partie commune ; suffixe = classe des octets observés (groupe alphanumérique pris en entier s'il est vu à 75 %) ; somme `mod36` détectée si elle est vraie pour tous les peer_id ; octets hors ASCII imprimable → `[\x00-\xff]` | selon le nombre d'octets par caractère distinct |
| `key` | longueur, alphabet, casse | idem ; **low** quand la key n'est vue qu'en UDP (entier de 32 bits : la casse hexadécimale n'existe pas) |
| Portées de renouvellement (peer_id et key) | `process` : tous les torrents d'une même vague de démarrage partagent la valeur ; `session` : la valeur d'un torrent change à sa reprise isolée ; `torrent` : elle ne change pas à cette reprise. Une vague = démarrages espacés de moins de 30 s ; une reprise isolée = une vague d'un seul torrent | high ; medium sans reprise isolée dans la capture ; low si la valeur change en cours de session |
| `numwant` par événement | valeur observée par événement ; manquant → recopié d'un événement voisin (low) ; paramètre absent → -1 | high / low |
| `headers` | ordre reçu, `Host` seulement s'il n'est pas en premier (entrée `{host}`), valeurs fixes ; en-tête absent de certaines requêtes ou à valeur variable → signalé | high / low |
| Famille et version | `User-Agent` (`Nom/version`), sinon préfixe de peer_id (`-qB5300-` → qbittorrent 5.3.0, confiance low) | high / low |

Limites connues : un client qui parle HTTP/1.0 ou n'est pas en `GET` est signalé (le client HTTP de seedmirage ne le reproduit pas ;
un `Host` qui n'est pas en premier est reproduit par l'entrée `Host` `{host}` de `headers`) ; l'option BEP 41 des annonces UDP
est déduite (`udp.urlData`) ; le renouvellement périodique d'un peer_id en cours de session (`ttlHours`)
est signalé mais pas modélisé ; `released` est la date de la capture, pas celle de la sortie du client ; la référence au code
source est celle de la version publiée du client, à compléter par les fichiers et lignes réellement lus.

Le profil produit est ensuite **rejoué** sur chaque requête capturée (auto-vérification : constat `selfcheck`), et passe
`emulation.Lint`.

## `capture.Diff`

```go
mismatches, err := capture.Diff(profile, rec, capture.DiffOptions{Private: true})
```

Compare une requête HTTP capturée à celle que produirait le profil pour le même torrent, événement et compteurs. Les valeurs
aléatoires sont neutralisées : `peer_id` et `key` doivent respecter le motif du profil (préfixe, alphabet, somme de contrôle) et
être encodés comme le profil encode ; tout le reste (ordre et valeur des paramètres, ordre, casse et valeur des en-têtes,
position de `Host`, `HTTP/1.1`) doit être identique à l'octet près. Liste vide = identique à l'aléa près.

`capture.DiffUDP(profile, rec, capture.DiffOptions{URLData: "/announce"})` fait de même pour une annonce UDP (BEP 15) : connection id
et transaction id sont neutralisés, le peer_id et la key (entier de 32 bits lu en hexadécimal) doivent suivre le motif du profil ;
l'action, l'info_hash, les compteurs, l'événement, l'adresse IP (0), `num_want`, le port et les options BEP 41 sont comparés à
l'octet près (l'option URLData n'est attendue que si le profil l'envoie, `udp.urlData`).

Le test `TestCapturedFixtures` rejoue les captures réelles de `internal/emulation/testdata/captures/` contre les
profils embarqués ; les écarts justifiés (valeur dépendant de la bibliothèque HTTP de l'installation) sont listés dans
`known-diffs.txt` à côté de la capture, tout autre écart fait échouer le test.
