---
status: archived — superseded by the convergent-repair / repeatable-Transfer pivot
opened: 2026-08-07
spec: ../protocol.md §4/R (R3, R7, R8, R9), §4/P (P4), §4/U (U3)
---

> **Archive note.** This plan describes the pre-pivot zone-authority work
> (epochs → ownership marks, S3 mirrored handoff sessions, silence reclaim).
> Production ownership now follows [ownership.md](../protocol/ownership.md):
> repeatable nominative Transfer (R6/R8), park + Claim (R10), and membership
> verdicts instead of silence. Citations below to deleted tests
> (`TestRefreshResolvesDuplicateZoneToNearest`,
> `TestDonorCrashBeforeTransferAckRestoresTheZone`,
> `TestCrashMidLeaveRestartsAsANormalMember`) and to S3 session RPCs are
> historical; see [guarantees.md](../protocol/guarantees.md) and
> [durability.md](../protocol/durability.md) for current pins.

# Autorité de zone : remplacer l'epoch par le bon mécanisme

## Pourquoi ce plan

Le protocole maintient deux ordres distincts (`protocol.md` §1b) :

- **l'ordre de placement** — qui doit posséder la zone `k` ? Réponse : le nœud
  vivant XOR-le-plus-proche. Auto-correcteur : R3 le rétablit à chaque Refresh.
- **l'ordre de fraîcheur** — laquelle de deux observations du résumé de `k` est
  la plus récente ? Réponse : celle qui vient du propriétaire actuel. **Pas**
  auto-correcteur : une mauvaise décision est absorbée dans l'agrégat parent et
  y reste.

Le champ `epoch` avait été introduit pour arbitrer la fraîcheur, et il servait à
arbitrer les deux. Il ne convenait ni à l'un ni à l'autre, pour une raison
structurelle et non pour un bug d'implémentation : c'était un compteur **local
au nœud** (`max(epochs)+1` sur toutes les zones que ce nœud détient) qui **ne
voyageait dans aucun payload de handoff**.

Deux propriétés indispensables étaient donc violées :

- **comparabilité inter-nœuds** — deux epochs produits par deux nœuds
  différents ordonnent en réalité « qui détient le plus de zones ».
- **survie au handoff** — après `A → B`, l'epoch de `B` est très souvent
  *inférieur* à celui de `A`, parce que `B` est typiquement un nœud fraîchement
  spawné dont le compteur démarre à 2 face à un donneur mature dans les
  dizaines.

La conséquence était permanente : le propriétaire du parent rejetait *tous* les
résumés que le nouveau propriétaire publierait, et son stub gelait sur la
dernière valeur pré-handoff du donneur. C'est le symptôme « parent bloqué à
1 024 items alors que ses enfants en totalisent 90 000 ». Comme chaque scale-up
PreferNear cède des zones d'un nœud mature vers un nœud neuf, le cas se
déclenchait à pratiquement chaque handoff vers un nœud spawné.

## Le principe retenu

**L'autorité plutôt que la version.** Un stub parent pour l'enfant `c` n'est
écrit qu'à partir d'une valeur servie par le propriétaire actuel de `c`.

`GET /aggregates` possède déjà exactement cette propriété : il ne répond que
pour les locations possédées localement et refuse de forwarder. Une réponse à
`/aggregates` est donc **auto-certifiante** — le fait de répondre *est* la
preuve de la propriété.

Sous ce régime :

- la comparabilité inter-nœuds devient sans objet : il n'y a qu'une autorité à
  la fois, donc rien à comparer ;
- la survie au handoff est acquise par construction : dès que `B` possède la
  zone, `A` cesse de répondre pour elle ;
- la tolérance au retard disparaît comme problème : un sous-comptage ne pouvait
  venir que d'un nœud ayant déjà délégué, ou d'un cache — deux sources qui
  cessent d'être admissibles.

Pour l'élection du propriétaire survivant en cas de duplicata, on n'invente pas
non plus d'ordre : c'est **l'ordre de placement** (R3) qui tranche.

Ce plan **supprime des mécanismes**, il n'en ajoute pas.

## Livré

### Étape 0 — épingler le défaut ✔

`TestEpochIsNodeLocalAndFreezesParentAfterHandoff` a reproduit le gel
(donneur epoch 9, receveur epoch 2, stub figé à 1 024) avant toute correction.
Le test a été réécrit en `TestParentStubFollowsOwnerAcrossHandoff`
(`domain/parent_stub_authority_test.go`) : il vérifie désormais la propriété
voulue plutôt que le défaut.

### Étape 1 — `/aggregates` seule autorité pour les stubs parents ✔

`Collection.Update` avait quatre appelants ; un seul était autoritaire.

| Appelant | Source | Autoritaire ? | Résultat |
|---|---|---|---|
| `applyPulledAbelian` | `/aggregates` | oui | conservé |
| `pullDelegatedAggregate` (repli `Get`) | `/set` | non | repli supprimé |
| `applyPulledSet` | `Get` (cache, path-fill) | non | n'écrit plus que le cache |
| `discoverRemoteChildren` | `/children` | non | ne fournit plus que l'existence |

`Collection.Update` est renommé `SetChildSummary` : la contrainte est portée par
le nom, pas par un contrôle interne. La méthode ne compare plus rien sinon
`IsEqual` (ne pas réécrire un stub identique).

Garantie **U3** : un stub parent suit le propriétaire courant de son enfant, y
compris quand celui-ci rétrécit légitimement (moitié de zone partie au split).
Tests : `TestParentStubFollowsOwnerAcrossHandoff`,
`TestParentStubIgnoresNonAuthoritativeSource`, `TestParentStubAcceptsOwnerShrink`,
`domain/summary_authority_sim_test.go`.

### Étape 2 — la double propriété se résout par l'ordre de placement ✔

**Cette étape a été livrée autrement que prévue, et le plan initial se trompait.**

Le plan prévoyait de garder `duplicate_zone.go` en remplaçant la comparaison
d'epochs par une élection XOR, et affirmait que « la détection de double
propriété devient gratuite » puisque toute réponse à `/aggregates` est une
revendication. Cette version a été écrite, puis mesurée : la détection coûte un
RPC `/aggregates` **par zone possédée et par pair connu**, à chaque tick. Sur
100 zones et 10 pairs, 1 000 RPC toutes les 10 secondes. Ce n'est pas gratuit,
c'est le fan-out le plus cher du protocole.

La question a donc été reposée : *qui a besoin de détecter le duplicata ?*
Personne. R3 donne déjà la zone au nœud XOR-le-plus-proche sans savoir que
quelqu'un d'autre la revendique, et un `Transfer` emporte tout le sous-arbre.
Vérifié expérimentalement avant suppression : deux nœuds possédant `aa` avec des
items divergents, liés, convergent en deux passes de Refresh vers un
propriétaire unique détenant l'union.

`duplicate_zone.go` est supprimé (231 lignes), ainsi que son pilote dans
`Update()` et ses compteurs. La garantie est reprise par
`TestRefreshResolvesDuplicateZoneToNearest` et
`TestConvergence_NoDoubleOwnership`.

### Étape 3 — supprimer le dispositif epoch ✔

Plus aucun consommateur. Supprimés : `Abelian.{epoch, Epoch, SetEpoch,
Dominates}`, `Collection.{epochs, Epoch, StampEpoch, BumpEpoch,
nextEpochLocked}`, le calcul dans `Set.Abelian()`, le champ de fil
`/aggregates` (`peer/contact.go`, `http/p2p/p2p.go`), et l'appel `StampEpoch`
sur le chemin de lecture client — qui prenait un verrou exclusif sur toute la
zone pour une simple lecture.

### Étape 5 — borner les fan-outs ✔

Une primitive unique remplace deux parcours non bornés :
`zoneCandidates(collection, location, max, skip...)` trie les pairs vivants par
distance XOR **à la clé de zone** et n'en rend que `max`. Trier par la clé est
ce qui rend un petit plafond suffisant : les propriétaires probables passent en
premier.

- `tryPeersGet` : `zoneFanout = 3` (l'α de Kademlia) en lecture client, 1 en
  refresh, au lieu de « tous les enregistrés puis tous les acquittés ».
- `pullDelegatedAggregate` : préféré (le receveur du handoff) puis les mêmes 3.

Garantie **K6**. Tests : `TestClientReadFanoutIsBounded`,
`TestRefreshReadAsksOneExtraPeerAtMost`,
`TestZoneCandidatesOrderedByDistanceToKey`.

### Étape 6 — nettoyages adjacents ✔ (partiel)

- `GetMultiple` résout jusqu'à `batchParallelism = 8` locations en parallèle :
  un lot froid de 96 coûte 12 allers-retours au lieu de 96 (**K7**).
- `Collection.List` et `TombstonesUnder` passent en `RLock`. `List` est appelé à
  chaque tick d'Update ; il ne bloque plus le plan de données.
- `syncParentsAfterHandoff` : rien à faire, le chemin classique appelait déjà
  `pullDelegatedAggregate` par clé à la fin de `transferToPeer`.
- `Collection.Complete` : `strings.Index(key, root) != 0` est sémantiquement
  identique à `!strings.HasPrefix(key, root)`. Rien à corriger.
- `Collection.Clean` et `BumpEpoch` : n'existent plus.
- Le paquet `traffic/` n'avait aucun test ; couvert à l'étape 4c.

### Hors plan — course sur l'arbre `owned` ✔

Découverte en passant la suite sous `-race` : `n.owned` stocke une
`map[domain.Key]any` par clé, et `BST.Get` en rendait la référence vivante.
`markOwnedDirtyForItem` (appelé à chaque item ingéré) la lisait **après** la
levée du RLock, pendant que `own` l'écrivait sous le verrou d'écriture — en Go,
une lecture/écriture concurrente de map est un `throw` du runtime, pas un
avertissement. Même défaut dans `removeOwnedKey`.

Correction : `BST.Read`, un accesseur qui exécute son callback sous le RLock, et
un test de emptiness déplacé à l'intérieur du callback d'`Update`. Le défaut
préexistait au travail en cours (vérifié sur un worktree au HEAD : 40
avertissements). Test : `TestOwnedTreeSurvivesConcurrentClaimsAndDirtyMarks`,
qui produit 12 courses si on annule la correction.

**La suite complète passe désormais sous `-race`**, Go et JS.

### Hors plan — revue des méthodes ajoutées ✔

Relecture de chaque méthode introduite depuis le dernier commit, en cherchant
les doublons d'un mécanisme existant. Consolidations :

| Supprimé | Remplacé par | Pourquoi |
|---|---|---|
| `Collection.isDirectChild` | `domain.IsDirectChild` | même prédicat, deux fois, deux tests homonymes |
| `Set.DropKey` | `Set.Delete` | identique, aux retours ignorés près |
| `Collection.ExportItems` | — | écrit pour la fusion de duplicatas, plus aucun appelant |
| `findRegisteredByName` + `findAcknowledgedByName` + une copie inline | `contactByName` (dans `membership.go`) | trois recherches par nom ; la nouvelle filtre sur joignable, ce que les trois appelants voulaient |
| `nearestLive` | inliné dans `find` | un seul appelant |
| `aggregatesError` + `errNoAggregates` | rien | aucun appelant ne distinguait « refus » de « n'en possède aucune » |
| 6 recopies de `MergeEncodings(BASE64, BASE64, location, collection)` | `zoneKeyID`, déplacé dans `placement.go` | l'inversion des arguments est un piège, une seule fois vaut mieux que six |

Renommages, pour que le rôle algorithmique se lise : `nearestFor` →
`zoneNearest` (famille `zoneKeyID` / `zoneNearest` / `zoneCandidates` /
`zoneFanout`), `serve` → `serveStale` (c'est **K4** qu'elle sert),
`pullAggregates` → `askAggregates` (vocabulaire *ask* pour le RPC, *pull* pour
la décision, *apply* pour l'écriture), `sealWALUnderBusy` → `sealWAL` (le seul
appelant énonce déjà la condition). `reconcileDelegatedParents` et
`discoverRemoteChildren` rendaient 5 et 2 entiers positionnels : ils écrivent
maintenant dans `updateStats`.

`parentSyncAt` était indexé par `collection+"|"+location` : `domain.Key` existe
et est comparable.

Gardés séparés à dessein : `find` (placement, pairs vérifiés seulement, échoue
si tous en quarantaine) et `zoneNearest` (lecture, n'échoue jamais) répondent à
deux questions différentes ; `Cache.Peek`/`LastTouch` n'ont pas d'appelant de
production mais sont les seules observations possibles de **K2** (un refresh ne
touche pas la LRU).

### Hors plan — deux défauts trouvés par la simulation ✔

La suite `app/simulation/` échouait, et pas de façon marginale : au HEAD elle
passe en 33 s, elle ne convergeait plus du tout. Deux causes indépendantes,
toutes deux introduites par le travail de cette session.

**1. `_` est une lettre de l'alphabet, pas un marqueur.** La clé réservée `"_"`
adoptée pour porter le total d'un `Set` sans liste d'enfants entre en collision
avec une location réelle : `IsDirectChild` rejetait la zone `_`, et `traverse`
passait donc à côté de tout ce qui y était rangé. Un nœud seul, 200 items,
`Count()` en voyait 197 — les trois manquants étaient en `_0`, `_1`, `_2`,
physiquement présents, comptés nulle part. Le total vit désormais sur l'agrégat
que `Set` tient déjà (`agg`), où rien ne peut le heurter ; les trois gardes
autour de `"_"` disparaissent. Garantie **P4**.

**2. Un `Transfer` ne fait pas de son destinataire un propriétaire.** La
réception appliquait les items directement dans la collection après un
`create()` inconditionnel, donc revendiquait la zone sur la seule parole du
donneur. Quand les deux vues du maillage divergent, la zone repart aussitôt et
revient : plus aucun point fixe, et un échantillon du total attrape toujours des
items en vol (789/800 à chaque mesure). La réception repasse par `insert`, qui
ne revendique que si le placement le désigne et forwarde sinon — tout en restant
synchrone et hors file d'attente, ce qui était l'intention légitime du
changement (un nœud plein, joignant, ou à file saturée ne doit jamais refuser
une zone). Garantie **R7**.

Le pair de simulation implémente désormais `GetAggregates` : sans lui, aucun
`/aggregates` ne circulait en simulation et la phase D n'y était jamais
exercée.

### Hors plan — revue de la suite de tests ✔

Onze fichiers de test disparaissent sans qu'aucune assertion ne soit perdue. Le
critère : un fichier de test porte le nom du fichier de production qu'il
interroge. Six fichiers décrivaient `read.go` sous des angles différents
(`read_cache`, `read_deep`, `read_stale`, `read_refresh`, `fanout`,
`routing_hint`) avec quatre stubs de pair pour la même chose — compter les
appels et rendre un `Set` fixe. Ils forment `core/read_test.go`, autour d'un
`readPeer` unique en `helpers_test.go` qui sépare les lectures client des
refresh. `resilience_test.go` ne correspondait à rien : ses tests rejoignent
`read_test.go` et le nouveau `core/update_test.go`. `duplicate_zone_test.go`
nommait un fichier de production supprimé en cours de session ; son test est
une question de placement, il rejoint `rebalance_test.go`. Côté `http/p2p`, six
fichiers pour un seul `p2p.go` n'en font plus qu'un, découpé par route.

`MergeEncodings(BASE64, BASE64, location, collection)` était recopié vingt fois
dans les tests, la même inversion d'arguments que côté production ; c'est
désormais `zoneID(t, collection, location)`, qui appelle `zoneKeyID`.

Deux tests n'assertaient rien de ce que leur nom annonçait.
`TestUpdateSkipsCachePullsWhileTransferBusy` vérifiait seulement que
`TransferBusy()` était vrai — jamais qu'un pull était évité. Il compte
maintenant les refresh reçus par le propriétaire, à zéro pendant le transfert
et non nuls une fois la session fermée.
`TestSyncParentsAfterHandoffAsksReceiverInOneBatch` n'avait aucune assertion :
il vérifie qu'un handoff de deux clés coûte un seul `/aggregates`, aucun
`/set`, et que les deux stubs suivent le receveur.

`domain/summary_authority_sim_test.go` est supprimé : ses 326 lignes
simulaient un maillage inventé sur place, et ses assertions portaient sur ce
modèle, pas sur le code. Ses quatre invariants sont tous tenus par des tests
réels (I1 `TestRefreshResolvesDuplicateZoneToNearest`, I2
`domain/parent_stub_test.go`, I3 `TestParentStubIgnoresNonAuthoritativeSource`,
I4 — qui manquait — `TestEmptyAnswerFromNearestIsAMissNotATruth`).

### Hors plan — un troisième défaut : l'ACK n'était plus durable ✔

Écrire la matrice de crash ligne par ligne a montré que la ligne « receveur,
après ACK » était fausse. Le passage du `Transfer` en synchrone (défaut 2
ci-dessus) l'avait cassée sans que rien ne le signale : `enqueue` écrivait une
ligne `ingress|` en WAL synchrone **avant** d'admettre l'écriture, `insert` ne
l'écrit pas. Restait la ligne nue produite par `add()`, que le rejeu ignore
quand la collection n'existe pas encore — c'est-à-dire exactement au démarrage
à froid. Un receveur acquittait donc un lot qui n'existait qu'en mémoire : le
donneur lâchait la zone, le receveur redémarrait, les items étaient perdus.

L'écriture WAL sort d'`enqueue` dans `logIngress`, et `Transfer` l'appelle
avant de placer. Une seule définition de « la trace durable d'une écriture
entrante », que les deux chemins d'admission partagent.
`TestReceiverCrashAfterAckReplaysTheBatch` l'épingle, avec les deux autres
lignes de la matrice qui n'avaient pas de test
(`TestDonorCrashBeforeTransferAckRestoresTheZone`,
`TestCrashMidLeaveRestartsAsANormalMember`).

### Hors plan — le chemin remplace le budget de sauts ✔

Une lecture profonde portait un budget : `DefaultDeepHops = 8`,
`RefreshHops = 3`, décrémentés à chaque forward. Le budget ne répond pas à la
question qu'il prétend traiter. Ce qu'on veut éviter, c'est qu'une lecture
tourne en rond — deux nœuds aux vues divergentes se nomment mutuellement
XOR-plus-proche, ce qui est l'état normal juste après la guérison d'une
partition — et un compteur ne détecte pas un cycle, il le laisse tourner huit
fois avant d'abandonner. Il coupe aussi une chaîne légitime de neuf nœuds.

La requête porte désormais `via`, la liste ordonnée des nœuds traversés
(`domain.Visited`, transmise en `via=n1,n2` sur `/set`). Chaque nœud s'y ajoute
avant de forwarder, refuse une requête qui le nomme déjà, et `zoneCandidates`
écarte les pairs qui y figurent — les interroger ne rapporterait qu'une
annulation. Le cycle meurt là où il se referme, et une chaîne longue va
jusqu'au bout. `len(via)` est en prime la distance réelle au client, donc ce
que le cache doit estampiller : `hops = 1` sauf si `hop <= 1` était une
approximation, c'est maintenant la mesure.

Le même raisonnement vaut pour `childrenMaxHop = 3` dans
`discoverRemoteChildren`, où il était déjà redondant : la boucle tenait un
`visited` par nom de pair. Le plafond ne faisait que tronquer une chaîne de
redirections valide.

Trois constantes et un réglage disparaissent (`INDEXUS_REFRESH_HOPS`,
`Settings.RefreshHops`). Dans `traffic/`, `hop_gt0` et `max_hop` deviennent
`forwarded` et `max_depth` : `max_hop` valait le budget restant, c'est-à-dire
8 en régime normal, et ne mesurait donc rien.

Tests : `TestDeepReadTerminatesOnATwoNodeRoutingCycle` monte le cycle à deux
nœuds réels et vérifie que chacun n'est sollicité qu'une fois ;
`TestGetCancelsWhenThePathReturnsToUs`, `TestGetSkipsPeersAlreadyOnThePath` et
`TestGetForwardsThePathItWalked` tiennent les trois moitiés du mécanisme ;
`TestDiscoverRemoteChildrenFollowsALongRedirectChain` et
`TestDiscoverRemoteChildrenStopsOnARedirectCycle` font la même paire côté
découverte. Garantie **K8** dans `protocol.md` §4/K.

Au passage, `TestControlCollectThenDelegate` échouait une fois sur huit :
il épinglait le pair candidat sur `zoneID(col, "aa")` alors que le nœud possède
`@`, et `control()` ne collecte que les clés du côté candidat de l'arbre XOR —
un tirage à pile ou face sur le nom aléatoire du nœud. Le pair est maintenant
épinglé sur une clé réellement possédée.

### Étape 4 — la session possède son rollback

`watchInboundTimeout` supprimait la session mais laissait les données et la
propriété que `applyZone → create() → own()` avaient installées. Le receveur se
retrouvait propriétaire d'une zone qu'on ne lui avait jamais accordée.

L'annulation est maintenant portée par la session. `cancelInbound` libère les
zones exactement comme le donneur les libère une fois le handoff confirmé —
`releaseZones`, une seule fonction pour les deux bouts — à l'expiration comme
sur un `applyZone` qui échoue en cours de route. Ce second cas était le même
défaut sur le chemin d'erreur : les zones déjà tirées restaient posées sans
session pour les reprendre. R3 cesse d'être le filet de rattrapage d'un défaut
de R6.

Le rollback et un ack tardif se croisent par construction. Les deux passent sous
`delegApplyMu`, et un `SwitchAck` visant une session déjà libérée est **refusé**
— sinon le donneur lâcherait la seule copie restante. Un ack pour une session
inconnue reste accepté : c'est une redélivrance, pas un rollback, et la refuser
abandonnerait la zone.

Garantie livrée — **R8 (rollback symétrique)**. Tests :
`TestInboundDelegationTimeoutReleasesZone`,
`TestSwitchAckAfterRollbackIsRefusedSoDonorKeepsTheZone`,
`TestSwitchAckForAnUnknownSessionIsAccepted`.

### Étape 4b — les nœuds ne se bloquent plus entre eux

En cherchant les blocages mutuels, trois trouvés, aucun n'étant un interblocage
de mutex : le code ne tient aucun verrou à travers un appel réseau. Ce sont des
cycles logiques.

**Le cycle de cutover.** `CaughtUp` se répond en appelant `SwitchAck` sur le
pair : une session dans chaque sens entre deux nœuds est un cycle d'appels, où
chacun attend l'autre depuis un handler. Un nœud ne tient plus jamais une
session entrante et une sortante avec le même pair — et comme un cycle exige
qu'un nœud soit à la fois donneur et receveur du même pair, cet invariant local
suffit à l'exclure globalement. Le sens est réservé sous `delegMu` **avant**
l'envoi de l'offre (`claimOutbound` / `claimInbound`) : la version qui
enregistrait la session au retour de l'appel laissait deux nœuds simultanés
trouver la place libre tous les deux. En cas d'égalité réelle, le plus petit nom
l'emporte — la même comparaison des deux côtés donne un sens, là où « je suis
occupé aussi » des deux côtés donne deux refus et une nouvelle collision au
Refresh suivant. Garantie livrée — **R9**.

**Le nœud tenu hors des rééquilibrages.** `Refresh` refuse de rééquilibrer tant
qu'une session entrante est ouverte. Un donneur mort en cours de handoff gelait
donc le receveur jusqu'à l'expiration, y compris pour le rééquilibrage qui
l'aurait soulagé. Une session périmée cesse de compter comme en cours dès son
échéance (`live()`), sans attendre que son watcher soit ordonnancé.

**Le martèlement entre voisins saturés.** `peerBusyWindow` était une pause fixe
de 100 ms : deux voisins qui se refusent leurs forwards se réessayaient à
cadence constante tant que durait la pression, sans qu'aucun ne se vide. La
pause double à chaque refus consécutif (100 ms → 2 s), et le premier écrit
accepté l'efface.

Tests : `core/deadlock_test.go`. Chaque scénario tourne sous échéance dure —
`mustFinish` vide la pile de toutes les goroutines au dépassement, ce qui
distingue un blocage d'un test lent. `TestMutualDelegationOffersDoNotInterlock`
surveille l'état interdit pendant tout l'échange plutôt que de l'échantillonner
à la fin, puis vérifie que le couple se stabilise, que rien n'est perdu et
qu'aucune zone n'a deux propriétaires.

### Étape 4c — le paquet `traffic/` ✔

Le paquet n'était qu'observationnel, donc classé à faible risque, et c'est
justement en l'écrivant que le risque est apparu. `traffic.Middleware` enveloppe
le mux : il enregistre `r.URL.Path` avant que le routeur décide si la route
existe, et la carte de compteurs est indexée par ce chemin. Un inconnu sur le
port P2P peut donc faire croître une carte mémoire sans borne, une entrée par
chemin inventé, sans être authentifié — l'authentification est vérifiée dans les
handlers, en aval. Un paquet d'observation n'a pas à être une surface d'attaque.

La correction garde ce qui rend le rapport lisible et borne le reste. `p2p`
déclare ses routes par `traffic.Register`, à partir de la liste qui enregistre
aussi les handlers : une route ne peut plus être servie sans être connue de ce
qui la compte. Une route déclarée garde toujours son compteur ; au-delà de
`maxPaths`, tout le reste partage un seul seau `other`. Le volume reste compté,
seule l'attribution se perd — et elle se perd pour les chemins que personne n'a
déclarés. Côté sortant les chemins sont des littéraux, donc déjà bornés.

`traffic/traffic_test.go` couvre l'inondation (la carte reste bornée, le total
reste juste), la route déclarée qui survit à l'inondation, la route déclarée
*après* elle, le statut/`via`/`refresh`, l'erreur de transport sans réponse, le
drainage entre fenêtres, et le compte concurrent pendant qu'un rapporteur draine
en dessous — sous `-race`, sans perte ni double compte.

## Validation restante

Les tests unitaires nommés ci-dessus sont verts, `-race` compris. La partition
explicite est faite : `Network.Partition` coupe le maillage en deux, les deux
moitiés continuent d'écrire, et `TestConvergence_PartitionRemergeKeepsTheUnion`
vérifie au remerge que le total est intact, que chaque zone a un propriétaire
unique et que chaque id écrit d'un côté se relit à travers le maillage.
`TestPartitionedReadIsAMissNotAnEmptyZone` tient l'autre bout, I4 : couper le
propriétaire d'une zone ne fabrique pas une zone vide, la lecture échoue ou
renvoie le contenu, jamais un zéro faisant autorité. Un contact de simulation
porte désormais l'identité de son détenteur (`ContactsOf`), sans quoi un lien
n'a pas de sens de coupure.

La vérification qui compte le plus était le chargement complet jusqu'au spawn
du deuxième nœud, en observant que l'agrégat racine `@` reste égal à la somme de
ses enfants de profondeur 1 pendant et après le handoff. Elle est désormais
automatisée par `TestRootAggregateNeverContradictsItsChildrenDuringHandoff` : un
nœud est chargé de 900 items étalés sur les enfants de profondeur 1, un second
le rejoint, et l'invariant est échantillonné en continu pendant que les zones
traversent — échantillonner à la fin ne dirait rien de la fenêtre, échantillonner
seulement pendant ne dirait rien de sa fermeture.

Ce que la mesure donne, sur huit exécutions sous `-race` : 116 à 176
échantillons par exécution, 0 à 3 désaccords en vol, et **zéro** à la
stabilisation. Le désaccord transitoire est la fenêtre du §7 vue depuis le
parent, entre le `Delegate` du donneur et l'application côté receveur ; ce qui
ne serait pas acceptable, c'est qu'il survive au handoff, parce qu'un agrégat
faux ne se corrige pas tout seul une fois sommé dans un parent. Le test tient
donc le maillage stabilisé à l'invariant pendant deux secondes plutôt que de
l'interroger une fois, et vérifie aussi qu'une zone a bien traversé — sans quoi
la surveillance n'aurait rien eu à observer.

Le risque résiduel de l'étape 1 était qu'un parent dont l'enfant n'a **aucun**
propriétaire joignable ne progresse plus du tout, là où il acceptait auparavant
une valeur approximative depuis un cache. C'est le comportement voulu — un stub
faux est pire qu'un stub en retard, et `TestParentStubHoldsWhenNoAuthorityAnswers`
le fixe — mais il fallait vérifier que trois candidats suffisent en pratique.
Le même test compte ces lectures où aucune autorité ne répond : **0 sur les huit
exécutions**, y compris pendant le handoff. Le blocage volontaire ne coûte donc
rien d'observable à cette échelle. La réserve honnête est que c'est mesuré à
deux nœuds : c'est la configuration où le handoff a lieu, ce n'est pas celle où
les trois candidats sont mis sous tension. Un maillage assez grand pour que les
trois plus proches d'une clé soient tous injoignables reste non mesuré, et c'est
là qu'il faudra regarder ensuite.
