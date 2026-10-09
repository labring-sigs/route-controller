# Route Controller

[![CI](https://github.com/labring-sigs/route-controller/actions/workflows/ci.yml/badge.svg)](https://github.com/labring-sigs/route-controller/actions/workflows/ci.yml)

Route Controller maintains IPv4 routes from standalone Kubernetes control planes into a Cilium cluster. It runs as a Static Pod on each control plane, routing Pod traffic through the owning worker and Service traffic through healthy workers using ECMP. Cilium workers handle forwarding and service load balancing.

## Requirements

- Linux, IPv4, and `NET_ADMIN` for local route management.
- Reachable worker InternalIPs through a common interface and directly connected subnet. Cilium must publish PodCIDRs and health addresses in `CiliumNode`.
- Worker IPv4 forwarding, compatible reverse-path filtering, and firewall/Cilium policies allowing control-plane traffic and health probes on TCP 4240.
- Cilium Helm value `bpf.lbExternalClusterIP=true` for external ClusterIP access. Both native routing and tunnel modes are supported.

The controller watches Node and Cilium Pod readiness, installs worker PodCIDR routes, then probes Cilium health responders before adding Service next hops. It refuses conflicting routes owned by another protocol and retains routes on shutdown. Reserve its route table/protocol pair exclusively for this deployment (default: table `254`, protocol `99`).

## Deployment

The [deployment script](deploy/scripts/standalone.sh) converts a control plane to standalone kubelet mode. It assumes a kubeadm/systemd layout whose kubelet `ExecStart` consumes `KUBELET_CONFIG_ARGS` and `KUBELET_KUBECONFIG_ARGS`; custom `ExecStart` overrides must be resolved first. Migrate ordinary workloads and verify network prerequisites before converting one control plane at a time, keeping API/etcd quorum available.

From the repository root, generate dedicated credentials on a management machine:

```bash
sudo bash deploy/scripts/standalone.sh kubeconfig \
  --admin-kubeconfig /path/to/admin.conf \
  --output /secure/controller.conf
```

Securely transfer the file to the target node. Credentials inherit the admin API endpoint, which must remain reachable after conversion. Generated tokens are long-lived; protect and rotate them. Static Pods cannot use ServiceAccount token injection.

On the target, select an image digest from a successful container workflow:

```bash
IMAGE='ghcr.io/labring-sigs/route-controller@sha256:<digest>'
sudo bash deploy/scripts/standalone.sh deploy \
  --kubeconfig /secure/controller.conf --image "$IMAGE"
sudo systemctl reboot

# After reconnecting:
sudo bash deploy/scripts/standalone.sh check
curl -fsS http://127.0.0.1:9919/readyz
```

Run the reboot command only after deployment succeeds. Deployment backs up local files, stops kubelet, and stages a separate kubelet configuration with API registration and HTTP/HTTPS listeners disabled. It leaves Cilium configuration unchanged. Verify PodIP, ClusterIP, webhooks, and API streaming operations before converting the next node.

The host credential file is `/etc/kubernetes/route-controller/kubeconfig`, mounted at `/etc/route-controller/kubeconfig` inside the container. The original kubelet configuration remains at `/var/lib/kubelet/config.yaml`; standalone mode uses a snapshot at `/var/lib/kubelet/standalone-config.yaml`.

To roll back:

```bash
sudo bash deploy/scripts/standalone.sh rollback
sudo bash deploy/scripts/standalone.sh check
```

Rollback stops the controller, flushes its routes, restores local files, and starts kubelet without requiring admin credentials or a reboot. Verify Node and Cilium recovery separately. Retained Nodes become NotReady during standalone operation; deleting them loses labels and taints that must be restored on rollback. Backups remain in `/var/lib/standalone-kubelet-manager`. For subsequent deployments, use a new `--state-dir` and pass the same path to `check` and `rollback`.

See `bash deploy/scripts/standalone.sh --help` for all options and recovery steps. The [Static Pod manifest](deploy/static-pod/image.yaml) and [RBAC](deploy/rbac/rbac.yaml) are also available for existing standalone installations.

## Configuration and monitoring

`route-controller run` discovers CIDRs from Cilium/kubeadm ConfigMaps and derives the interface and worker subnet from Node addresses and local routes. Override these with flags or the [example configuration](config/examples/route-controller.yaml). Precedence is flags, `ROUTE_CONTROLLER_` environment variables, YAML, then defaults.

```bash
route-controller run --help
route-controller validate --config config/examples/route-controller.yaml
route-controller version --output=json
```

`validate` runs offline and requires complete route settings. Default endpoints:

| Address | Endpoint | Purpose |
| --- | --- | --- |
| `127.0.0.1:9918` | `/metrics`, `/status` | Metrics and current routing state |
| `127.0.0.1:9919` | `/healthz`, `/readyz` | Process health and routing readiness |

## Development

```bash
make build
make verify
# Linux with root/NET_ADMIN; creates temporary interfaces and routing tables:
sudo make integration-test
```

CI runs race tests, static analysis, privileged integration tests, vulnerability checks, manifest validation, and Linux amd64/arm64 builds. Report vulnerabilities through the process in [SECURITY.md](SECURITY.md).
