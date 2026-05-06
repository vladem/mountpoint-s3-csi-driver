# Manual Testing — Daemonset Mode

## Prerequisites

- An EKS cluster (e.g., `al2023-t3-medium-cluster` in `us-west-2`)
- An ECR repository (e.g., `<>.dkr.ecr.us-west-2.amazonaws.com/s3-csi-driver`)
- `kubectl` configured and aliased (`k` for kubectl, `ks` for `kubectl -n kube-system`)
- Docker, helm, AWS CLI

## Configuration

```bash
export TAG=daemonset-mode-8
export REGISTRY=<>.dkr.ecr.us-west-2.amazonaws.com
export IMAGE=$REGISTRY/s3-csi-driver
export REGION=us-west-2
export CLUSTER=al2023-t3-medium-cluster
```

## Build & Push

```bash
PLATFORM=linux/amd64 IMAGE=$IMAGE TAG=$TAG make build_image

aws ecr get-login-password --region $REGION | docker login --username AWS --password-stdin $REGISTRY

docker push $IMAGE:$TAG
```

## Install (Daemonset Mode)

```bash
helm upgrade --install aws-mountpoint-s3-csi-driver charts/aws-mountpoint-s3-csi-driver \
    --namespace kube-system \
    --set image.repository="$IMAGE" \
    --set image.tag="$TAG" \
    --set image.pullPolicy="Always" \
    --set mounterMode="daemonset"
```

## Verify Pods

```bash
k get pods -A | grep s3-csi
# Expect: s3-csi-node-XXXXX (Running), s3-csi-mounter-XXXXX (Running)
# No controller pod, no mount-s3 namespace pods
```

## Test Mount

```bash
k apply -f specs/static_provisioning_deployment.yaml
k get pods -w
# Expect: s3-app-deployment pod reaches Running

# Check logs
ks logs -l app=s3-csi-mounter
ks logs -l app=s3-csi-node -c s3-plugin
```

## Test Unmount

```bash
k delete -f specs/static_provisioning_deployment.yaml
k get pods
# Expect: pod terminated cleanly
```

## Uninstall

```bash
helm uninstall aws-mountpoint-s3-csi-driver --namespace kube-system
```

## Troubleshooting

```bash
# CSI node logs
ks logs -l app=s3-csi-node -c s3-plugin --tail=50

# Mounter daemonset logs
ks logs -l app=s3-csi-mounter --tail=50

# Describe stuck pod
k describe pod <pod-name>

# Check mounter pod comm dir is accessible from csi-node
ks exec <s3-csi-node-pod> -c s3-plugin -- ls /var/lib/kubelet/pods/
```
