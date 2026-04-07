# Kubernetes Examples

Plain Kubernetes manifests for deploying Sortie to a cluster. For the full
guide — secrets, networking, storage classes, monitoring — see
[Deploy Sortie to Kubernetes](https://docs.sortie-ai.com/guides/deploy-sortie-to-kubernetes/).

## Prerequisites

Build an agent-specific image using the Dockerfiles in `examples/docker/`:

```sh
docker build -f examples/docker/claude-code.Dockerfile -t sortie-claude .
```

Push it to a registry your cluster can pull from, then update the `image`
field in `deployment.yaml`.

## Quick start

Create a Secret with your API keys:

```sh
kubectl create secret generic sortie-secrets \
    --from-literal=ANTHROPIC_API_KEY="sk-..." \
    --from-literal=SORTIE_JIRA_API_KEY="..." \
    --from-literal=SORTIE_JIRA_ENDPOINT="https://your-org.atlassian.net" \
    --from-literal=SORTIE_JIRA_PROJECT="PROJ"
```

Apply the manifests:

```sh
kubectl apply -f examples/k8s/
```

Verify the pod is ready:

```sh
kubectl get pods -l app.kubernetes.io/name=sortie
```

## Manifest files

| File | Description |
|---|---|
| `deployment.yaml` | Single-replica Deployment with Recreate strategy |
| `configmap.yaml` | Sample WORKFLOW.md mounted into the container |
| `service.yaml` | ClusterIP Service exposing port 7678 |
| `pvc.yaml` | 1Gi ReadWriteOnce PVC for the SQLite database |

## Customization

1. **Image** — replace `sortie-claude:latest` in `deployment.yaml` with your
   registry image (e.g., `registry.example.com/sortie-claude:v1.0.0`).
2. **Workflow** — edit the `WORKFLOW.md` content in `configmap.yaml` to match
   your tracker and agent setup.
3. **Secrets** — the Deployment references a Secret named `sortie-secrets`.
   Add any environment variables your workflow requires.
4. **Storage** — adjust the PVC size or storage class to match your cluster.
