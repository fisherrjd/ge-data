# ge-data on k3s

Deploys the TimescaleDB + Go ingester stack to a k3s cluster. This is the
*deployed* target; `default.nix` remains the local dev DB and `docker-compose.yml`
is the single-host Docker target. All three load the same `init/01_schema.sql`.

## Topology

```
namespace: ge-data
├── Secret       ge-data-db        POSTGRES_PASSWORD + USER_AGENT  (NOT committed)
├── ConfigMap    ge-data-schema    generated from ../init/01_schema.sql by kustomize
├── StatefulSet  ge-data-db        TimescaleDB, 1 replica, PVC (volumeClaimTemplates)
├── Service      ge-data-db        headless (clusterIP: None), :5432
└── Deployment   ge-data-ingester  Go poller, replicas:1, Recreate
```

Two separate pods, not one. The DB is stateful and long-lived (StatefulSet + PVC);
the ingester is stateless and redeployed often (Deployment). They talk over the
cluster network by Service DNS, never localhost.

## Why the ingester is pinned to 1 replica

It's a timed poller that writes. The 1m `/latest` dedup-on-change path is racy under
concurrency, so >1 replica risks double inserts. `replicas: 1` + `strategy: Recreate`
guarantees a single poller even mid-rollout. Don't add an HPA. For HA later, use a
leader-election Lease, not more replicas.

## Deploy

```bash
# 1. Secret (out of band — secret.yaml is gitignored)
cp k8s/secret.example.yaml k8s/secret.yaml
$EDITOR k8s/secret.yaml
kubectl apply -f k8s/secret.yaml

# 2. Build + push the ingester image, then set it in ingester.yaml
#    (image: ge-data-ingester:latest -> your-registry/ge-data-ingester:<tag>)
docker build -t <registry>/ge-data-ingester:<tag> ./ingester
docker push <registry>/ge-data-ingester:<tag>

# 3. Everything else via kustomize (generates the schema ConfigMap from init/).
#    --load-restrictor LoadRestrictionsNone is REQUIRED: the schema lives in
#    ../init, outside the k8s/ kustomize root, and the default restrictor blocks
#    reading above-root files. (ArgoCD: set kustomize.buildOptions accordingly.)
kubectl apply -k k8s/ --load-restrictor LoadRestrictionsNone

# 4. Watch it come up
kubectl -n ge-data get pods -w
kubectl -n ge-data logs deploy/ge-data-ingester -f
```

## Connect for a poke-around

```bash
kubectl -n ge-data port-forward svc/ge-data-db 5000:5432
psql "postgresql://ge-data:<pw>@localhost:5000/ge-data"   # \dx, \d+ prices_5m
```

## Schema changes / migrations

The ConfigMap → `/docker-entrypoint-initdb.d` path runs **only on first boot of an
empty PVC**. Editing `init/01_schema.sql` will NOT re-apply to an existing DB. Apply
later changes by hand:

```bash
kubectl -n ge-data exec -it ge-data-db-0 -- \
  psql -U ge-data -d ge-data -f /docker-entrypoint-initdb.d/01_schema.sql   # only safe on fresh DB
# real migrations: port-forward + psql -f init/02_*.sql, or adopt golang-migrate later
```

To wipe and start clean (DESTROYS DATA): delete the StatefulSet's PVC
(`kubectl -n ge-data delete pvc pgdata-ge-data-db-0`) and re-apply.
