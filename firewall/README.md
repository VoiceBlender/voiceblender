# VoiceBlender Docker firewall

Restricts the **control/signalling** surfaces to an explicit allowlist of source IPs while
keeping **media** and **container-to-container** traffic open.

| Surface          | Port                 | Default |
|------------------|----------------------|---------|
| REST API + VSI   | tcp/8080             | restricted |
| SIP              | udp/5060             | restricted |
| SIP over TLS     | tcp/5061             | restricted |
| RTP / media      | udp/10000–20000      | **open** |
| MoQ (if enabled) | udp/8443             | **open** |

## How it works

Docker publishes container ports via DNAT, so the traffic traverses the `FORWARD` path — not `INPUT` —
and ordinary `INPUT` firewall rules never see it. Docker provides the **`DOCKER-USER`** chain, which it
evaluates *before* its own published-port rules and **never flushes** on container restart/recreate or
daemon restart.

We hang a dedicated **`VB-FILTER`** sub-chain off `DOCKER-USER`. Rules only match traffic arriving on the
host's **external interface** (`-i <ext_if>`), so traffic between containers (which arrives on the Docker
bridge) is never touched. Ports are matched **container-side** (post-DNAT), so the cluster's host ports
8081/8082 → container 8080 are covered automatically. Host-local access (`localhost`) traverses `OUTPUT`,
not `FORWARD`, and is unaffected.

```
external pkt ──▶ FORWARD ──▶ DOCKER-USER ──▶ VB-FILTER ──▶ source allowed? RETURN (accept)
                                                          └▶ otherwise      DROP
```

## Files

- `voiceblender-docker-user.rules` — the allowlist, in **iptables-save / iptables-restore** format. Edit
  the `-s <ip/cidr> ... -j RETURN` lines. `__EXT_IF__` is substituted at apply time.
- `vb-firewall.sh` — idempotent loader (`apply` / `status` / `remove`).
- `vb-firewall.service` — systemd oneshot for host-reboot persistence.

## Usage

1. Edit the allowlist:

   ```bash
   $EDITOR firewall/voiceblender-docker-user.rules
   ```

   Add one `RETURN` line per allowed source for each port you expose, e.g.:

   ```
   -A VB-FILTER -i __EXT_IF__ -p tcp --dport 8080 -s 198.51.100.0/24 -j RETURN
   ```

2. Apply (auto-detects the external interface from the default route):

   ```bash
   sudo firewall/vb-firewall.sh apply
   sudo firewall/vb-firewall.sh status
   ```

   Pin the interface(s) explicitly if needed:

   ```bash
   sudo VB_EXT_IF="eth0 eth1" firewall/vb-firewall.sh apply
   # or
   sudo firewall/vb-firewall.sh apply --iface "eth0 eth1"
   ```

3. Tear down:

   ```bash
   sudo firewall/vb-firewall.sh remove
   ```

## Persistence

- **Container restart / recreate** and **Docker daemon restart**: automatic — `DOCKER-USER` (and our
  `VB-FILTER` chain + jump) are preserved by Docker.
- **Host reboot**: install the systemd unit, which re-applies the rules after `docker.service` starts:

  ```bash
  sudo cp firewall/vb-firewall.service /etc/systemd/system/
  # ExecStart in the unit points at /opt/voiceblender/firewall/vb-firewall.sh — edit if your
  # checkout lives elsewhere.
  sudo systemctl daemon-reload
  sudo systemctl enable --now vb-firewall.service
  ```

## IPv6

If Docker IPv6 is enabled, mirror the rules for `ip6tables`: copy the `.rules` file with IPv6 sources and
load it with `ip6tables-restore` (the same `DOCKER-USER` mechanism applies under `ip6tables`).

## Verify

```bash
# From a host NOT in the allowlist — should time out:
curl --max-time 3 http://<host>:8080/v1/rooms

# Container-to-container still works:
docker compose -f docker-compose.cluster.yml up -d
docker exec sbc curl -s http://dialer:8080/v1/rooms

# Rules survive a container recreate:
docker compose up -d --force-recreate
sudo iptables -S VB-FILTER
```
