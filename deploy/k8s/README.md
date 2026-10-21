# Deploying easydev-platform on k3s (plain Kubernetes YAML)

No Helm. These are plain manifests: `kubectl apply -f deploy/k8s/`.

> This is **deployment packaging**, unrelated to easyops' (removed) Helm
> *release* feature and unrelated to easyblobs' Helm *package registry*
> protocol (`/helm/...`, which stores users' chart packages).

## What gets created

| File | Contents |
|---|---|
| `00-namespace.yaml` | the `easydev` namespace |
| `10-secrets.yaml` | admin token, easyops service token, DB DSN, S3 keys (template — fill in) |
| `20-postgres.yaml` | Postgres (StatefulSet + Service + PVC). Optional: point `EASYVCS_DB_DSN` at a managed DB and skip this. |
| `30-easyvcs.yaml` | easyvcs Deployment + Service |
| `40-easyblobs.yaml` | easyblobs Deployment + Service |
| `50-easyops-rbac.yaml` | easyops ServiceAccount + Role/RoleBinding (the ONLY component needing k8s RBAC) |
| `51-easyops.yaml` | easyops Deployment + Service |
| `60-platform.yaml` | easydev-platform Deployment (gateway + CI) + Service |
| `70-ingress.yaml` | an Ingress that publishes the platform on ONE origin |

## Order

```sh
kubectl apply -f deploy/k8s/00-namespace.yaml
kubectl apply -f deploy/k8s/10-secrets.yaml      # edit the placeholders first
kubectl apply -f deploy/k8s/20-postgres.yaml     # optional
kubectl apply -f deploy/k8s/30-easyvcs.yaml
kubectl apply -f deploy/k8s/40-easyblobs.yaml
kubectl apply -f deploy/k8s/50-easyops-rbac.yaml
kubectl apply -f deploy/k8s/51-easyops.yaml
kubectl apply -f deploy/k8s/60-platform.yaml
kubectl apply -f deploy/k8s/70-ingress.yaml
```

## The convenient alternative: ONE pod

`easydev-platform` **embeds easyvcs** and can proxy blobs/ops. If you do not
need to scale the components separately, deploy **only** `60-platform.yaml`
with `EASYDEV_BLOBS_URL` / `EASYDEV_OPS_URL` unset (or pointing at the same
namespace's services) — that is a single-pod platform.

## Individually deployable

Every component is its own Deployment: you can drop `40-easyblobs.yaml` or
`51-easyops.yaml` and the platform still boots (its `/blobs` or `/ops` prefix
returns 502). Same in reverse: run only `easyvcs` and point an existing CLI at
it — nothing in easyvcs requires the platform.
