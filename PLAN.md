# ZFS agent sandboxes: build plan

Oct 26, 2026 · @Liam · updated Oct 27, 2026

## Goal and principles

Build a small system that creates, forks, pauses and moves sandboxes in under a second, on any machine in your tailnet that has a ZFS pool. It turns the SQL Server branching demo into a sandbox platform that agents (or anything else) can use.

The product is the sandbox. How agents are orchestrated, how sessions are presented, and how work flows into PRs belong to whatever client sits on top (T3Code, a script, an agent with the MCP tools), not to this project.

- **A host is a Linux machine in the tailnet with a ZFS pool.** Nothing else is assumed, and no junkyard of mixed machines.
- **Every sandbox is a ZFS clone of a template snapshot.** Creating and forking cost almost nothing until something writes.
- **ZFS is the source of truth.** Sandbox metadata lives in ZFS user properties, so a host can rebuild its state from `zfs list` after a crash.
- **Three ways in, one API.** A CLI, an API and an MCP server, all exposing the same operations. The CLI and MCP server are thin clients of the API.
- **Measure everything.** The benchmarks are the deliverable, not just the code.
- **Trusted compute only.** Your machines, your workloads.

## Architecture

With one host there is no control plane: clients talk to `sbxd` directly, and `zfs list` is the registry. With a second host, a small coordinator takes over the jobs that span hosts. Clients talk to "an sbx endpoint" and see the same API either way, so nothing changes on the client when the coordinator arrives.

&#91;embedded content: architecture · a CLI and two independent ZFS hosts\]

- **Interfaces.** The API is the contract. `sbx` (CLI) runs on your laptop; `sbx mcp` serves the same operations as MCP tools for any agent that supports them. Both call one sbx endpoint: `sbxd` on a single host, the coordinator once there are several.
- **`sbxd` host daemon.** One Go binary per host. It embeds Tailscale with `tsnet`, so it is its own node in the tailnet, and it uses Tailscale's identity lookup for auth instead of building its own.
- **Coordinator (from milestone 4).** Another `tsnet` node with SQLite, small enough to run on the desktop at first. It exposes the same API as `sbxd` and proxies `exec` to the right host. It owns the template registry, the sandbox-to-host index (including the last-seen state of offline hosts) and placement. Multi-step procedures (moves, leases, template rollout and drain) run as Temporal workflows alongside it; see [TEMPORAL.md](TEMPORAL.md).
- **Who is authoritative.** Hosts are authoritative for facts: which datasets and snapshots exist, and whether each container is running. The coordinator is authoritative only for intent and for what no single host can know. It rebuilds its sandbox index from hosts, so it never contradicts ZFS. If it is down, the global view is lost but each host still serves direct requests.
- **Identity.** A template version is identified by its snapshot's `guid`, recorded as a property from milestone 2. That is how you check that two hosts share the exact same snapshot. Sandbox IDs are globally unique (ULIDs) so per-host indexes merge without collisions.
- **ZFS layout.** Templates live at `tank/sbx/templates/<name>` with a snapshot per version (`@v1`). Sandboxes live at `tank/sbx/sandboxes/<id>`, each a clone of a template snapshot.
- **Templates are root filesystems.** `sbxd` pulls a base image by digest and flattens it into the template dataset itself (in Go, with `go-containerregistry`, the library behind `crane export`), snapshots that as `@base`, then runs the recipe's setup steps (clone the repo, install dependencies) in a temporary build sandbox on the dataset, and snapshots the result. Build steps get the same runtime and network isolation as any sandbox, so the step that runs `npm install` and its postinstall scripts is never the least isolated thing on the host. There is no buildah or BuildKit: no second image store, and no other tool writing firewall rules. A rebuild clones from `@base` rather than pulling again. A sandbox is the whole machine's disk, not just a workspace folder.
- **Template recipe.** Each template is built from a recipe: a small sbx-specific manifest naming a base image pinned by digest and a list of setup steps. A Dockerfile is optional and lives outside `sbxd`: build and push it with anything, then reference the result by digest. The manifest also declares what a sandbox of it gets: CPU and memory limits, disk quota, ports to expose, and which secrets to inject at create time. The recipe, with the image digest recorded as `sbx:image-digest`, is what makes a template reproducible.
- **Runtime.** Version 1 runs each clone as a container root filesystem with `runc` (or `crun`, which takes the same OCI spec and commands; the runtime binary is a config setting). `sbxd` generates each sandbox's OCI `config.json` (rootfs is the clone's mountpoint, cgroup limits from the recipe, a tiny init as PID 1) and creates and owns the sandbox's network namespace, veth pair and nftables rules. There is no Podman: no second container database, and no other tool writing firewall rules, so ZFS stays the only durable record and runc's state lives in `/run`. Exec is `runc exec`, with `sbxd` holding the pipes. All of this sits behind a small Go `Runtime` interface (start, freeze, thaw, exec, stop, list) so version 2 can add Firecracker microVMs on hosts with KVM, using ZFS volumes (zvols) as their disks.
- **Metadata.** ZFS user properties on each dataset, such as `sbx:template`, `sbx:template-guid`, `sbx:image-digest`, `sbx:parent`, `sbx:created`, `sbx:ttl` and `sbx:state` (`creating`, `running` or `paused`: the intended state, which `sbxd` restores after a restart).

## Sandbox boundaries

What a sandbox can reach and what it holds. These are defaults; a template can loosen them explicitly.

- **Network: isolated by default.** Egress to the internet only. No traffic to other sandboxes, the host, or anything on the tailnet. Every host is a tailnet node, so without this a sandbox could reach the coordinator or other hosts. Management never needs a sandbox on the tailnet: clients go through `sbxd`, which reaches its sandboxes locally.
  - **Rules live on the host.** One nftables table owned by `sbxd`, matched on the host end of each sandbox's veth, never inside the sandbox's network namespace, where anything with `CAP_NET_ADMIN` could delete them. Host-side veths are named `sbx-<id>` so one wildcard rule covers every sandbox.
  - **Routed veths, not a bridge.** Each sandbox gets its own point-to-point link (a `/31` from a host pool) and a host route, so every packet it sends, including to another sandbox, is routed through the forward chain and matched on an interface the sandbox can't change. A shared bridge would switch sandbox-to-sandbox traffic at layer 2, past the forward chain, and need `br_netfilter` or a second rule set to stop it. Firecracker's tap devices in version 2 are routed the same way.
  - **Forward chain: an allowlist.** Traffic from a sandbox veth is accepted only if it is IPv4 to a non-private destination (not `100.64.0.0/10`, RFC 1918, link-local, loopback or multicast); everything else is dropped, so anything not thought of is denied by default.
  - **Input chain: closed.** A packet addressed to any of the host's own addresses (its LAN or tailnet IP, a veth gateway) goes to the input chain, not forward. The table drops every new connection arriving on a sandbox veth and allows only replies to connections the host opened.
  - **IPv4 only.** IPv6 from a sandbox is dropped by the allowlist; otherwise the tailnet's IPv6 range (`fd7a:115c:a1e0::/48`) would be reachable whenever IPv6 forwarding is on. `disable_ipv6` inside the namespace only stops programs stalling on IPv6; it isn't the boundary.
  - **DNS.** A public resolver is written into each sandbox's `/etc/resolv.conf` at create. Never the host's resolver or Tailscale's MagicDNS, so the host stays closed.
  - **Order of setup.** `sbxd` installs the table before it enables IPv4 forwarding, and refuses to start sandboxes if the table is missing.
- **Secrets: injected, never baked.** Secrets named in the template recipe are injected at create time. Which secrets those are, and how narrowly they're scoped, is the client's concern.
- **Ports.** Ports declared in the template are reachable from your tailnet through `sbxd` (forwarded over `tsnet`), so a sandbox can run a dev server or a preview environment. Pairs naturally with branchable SQL Server databases for per-PR previews.
- **Resources.** `refquota` caps disk; cgroup limits from the template recipe cap CPU and memory. Idle sandboxes auto-pause (no exec or port traffic for a set period) and resume on the next request, in addition to TTL expiry.

## Core operations

Every operation is a thin wrapper over a few ZFS commands plus a container start or stop, so most of them take well under a second. Each one is available identically through the CLI, the API and MCP.

| Operation | What sbxd does | ZFS underneath | Cost |
| --- | --- | --- | --- |
| Build template | Pull and flatten a base image, run setup steps (clone the repo, install dependencies) in a build sandbox, snapshot | `zfs create`, then `zfs snapshot .../templates/app@v1` | Minutes, once per template version |
| Create | Clone the template snapshot, inject secrets, start a container on it | `zfs clone .../app@v1 .../sandboxes/<id>` | Clone is instant; container start dominates |
| Exec, attach and files | Run commands, stream output, read and write files over the tailnet API | None | One network round trip per call |
| Fork N ways | Freeze the sandbox, snapshot it, clone it N times, unfreeze | `zfs snapshot .../<id>@fork1`, then `zfs clone` N times | Instant; each fork pays only for its own writes |
| Pause | Stop the container, snapshot the dataset | `zfs snapshot .../<id>@pause` | Instant; keeps disk state only, not running processes |
| Resume | Start a container on the same dataset | None | Container start |
| Move to another host | Ship only the blocks changed since the template | `zfs send -i @v1 .../<id>@pause`, piped to `zfs receive` on the other host | Proportional to the sandbox's changes, not its size |
| Destroy | Stop the container, destroy the dataset; clean up unused templates | `zfs destroy` | Instant |

Freezing before a fork (`runc pause`, which writes to the sandbox's cgroup freezer) stops every process at the same instant, so the snapshot is consistent across processes. Without it, the snapshot is crash-consistent, which the SQL Server demo showed is still recoverable.

### Restarts and reboots

- **`sbxd` restarts, host stays up.** Sandboxes keep running: they are started detached (`runc create` then `runc start`, init's stdio to a log file), and each network namespace is pinned at `/run/sbx/netns/<id>`, so the veths, routes and nftables table live in the kernel, not in `sbxd`. On startup `sbxd` adopts what exists, from `zfs list`, `runc list` and the live veths. Execs in flight are lost, because `sbxd` held their pipes: the client sees "connection lost, exit status unknown", never success.
- **Host reboots.** `/run` is wiped, so runc state, namespaces, veths, the nftables table and every process are gone, while the datasets survive. That is the same as a pause. On boot `sbxd` reinstalls the table, enables forwarding, and starts a fresh container on every dataset whose `sbx:state` is `running`, the same code path as resume. Sandboxes may get new IPs; nothing outside `sbxd` records them.
- **Crash mid-operation.** `sbxd` sets `sbx:state=creating` before anything else and `running` at the end. On startup it removes any runc container, veth, namespace or bundle directory without a matching dataset, and destroys datasets still marked `creating`. This is the first cut of the orphan reconciler that milestone 3's reaper grows into.

## Milestones

Each milestone ends in something you can run and measure, and stopping after milestone 4 already gives you a working, demoable system.

0. **Done: ZFS branching demo.** ZFS pool on the Arch machine, SQL Server branched and verified independent.
1. **Manual fork demo.** Build a template dataset by hand from one of your real repos with its dependencies installed. Snapshot it, clone it five times, and run the test suite in each clone with `runc run`, using a hand-written OCI spec and a network namespace set up by hand. Time container start with both `runc` and `crun` on the same clone.
   - Done when: five sandboxes run tests in parallel, `zfs list` shows what each one cost, and you have start-time numbers for `runc` and `crun`.
2. **sbxd v0 on one host.** A Go daemon, its API and the CLI with create, list, exec, files and destroy. Templates built from a recipe, metadata in ZFS properties (including the template snapshot's `guid`), ULID sandbox IDs, served on localhost only. Default network isolation and CPU, memory and disk limits from the recipe. Shape the API as the single endpoint the coordinator will later expose too.
   - Done when: `sbx create app` then `sbx exec <id> npm test` works end to end, and the sandbox can reach the internet but not the tailnet.
3. **Fork, pause, resume and cleanup.** Freeze-and-fork, pause and resume, idle auto-pause, and a reaper that destroys sandboxes past their `sbx:ttl` and unused template versions. The reaper is a plain loop on each host; it later becomes the orphan reconciler.
   - Done when: you fork a running sandbox five ways mid-task, keep one, and the rest disappear on their own.
4. **Tailnet, a second host, the coordinator and Temporal.** Move the API onto the tailnet with `tsnet` and Tailscale identity, including port forwarding. Add the coordinator: template registry keyed by snapshot `guid`, a sandbox index rebuilt from hosts, simple placement (has the template, then least loaded). Bring in Temporal here: the move workflow first (receive on the destination, then destroy on the source, with compensation), then per-sandbox lease workflows that take over TTLs from the reaper. Ship templates to new hosts with `zfs send`, and move paused sandboxes with incremental sends.
   - Done when: a sandbox paused on the desktop resumes on a cheap Hetzner box through the coordinator, the bytes sent roughly match the sandbox's changes, and `sbx list` still shows paused sandboxes while their host is offline.
5. **MCP server.** `sbx mcp` exposes the core operations as MCP tools, as a thin client of the API. No agent-specific logic.
   - Done when: an MCP-capable agent can create, exec in, fork and destroy a sandbox using only the tools.
6. **Benchmarks write-up.** Publish the numbers from the next section with a short explanation of where the time goes.
7. **Stretch: microVMs.** Firecracker on hosts with KVM, zvol clones as disks, then memory snapshots so forks start with processes already running. This is also what lets one sandbox run a multi-container stack (app plus SQL Server) with its own container daemon.

## What to measure

These numbers are the portfolio piece: each one answers a question an infrastructure interviewer would ask about the design.

| Metric | How to measure | Why it matters |
| --- | --- | --- |
| Create to first command output | Time from `sbx create` to the first byte of `echo ok` | The latency a caller actually feels |
| Fork latency at 1, 5 and 20 forks | Time from fork request until every fork accepts commands | Shows forking stays flat as N grows |
| Space per sandbox | `used` from `zfs list` after a typical task | Proves the M + 2N cost model on real work |
| Exec round trip | Median and slowest 1 in 100 for a no-op command over the tailnet | Agents make hundreds of calls per task |
| Pause and resume time | Each step separately, with a cold and a warm host | The cost of parking idle sandboxes |
| Move cost | Bytes sent and seconds taken versus the sandbox's full size | Shows incremental sends beat full copies |
| Crash test | Kill sbxd or the host mid-snapshot, then check what survives | Where crash consistency stops being theory |

Record each with the time spent in every step (ZFS, container runtime, network), not just the total.

## Gotchas to know up front

The biggest one: build each template once and distribute it with `zfs send`, never rebuild it per host, or incremental moves stop working.

- **Incremental sends need shared history.** `zfs send -i` only works if the receiving host has the exact same snapshot, received from the sender, not an identical-looking rebuild.
- **Clones pin their template.** ZFS refuses to destroy a snapshot that still has clones. Cleanup must check each template's `clones` property first, and `zfs promote` can flip the dependency when needed.
- **Cap every sandbox.** Set `refquota` on each sandbox so a runaway process can't fill the whole pool.
- **Snapshots hold deleted data.** Space freed inside a sandbox isn't returned while a snapshot still references it, so stale fork and pause snapshots need cleaning up too.
- **Never bake secrets into a template.** Every clone and every send carries them. Inject them at create time instead.
- **Hosts are tailnet nodes.** A sandbox with default networking can reach whatever its host can. Block the tailnet and private ranges for every sandbox, on the host side, before anything else.
- **Forks copy everything on disk, including identities.** Anything with per-machine identity inside a sandbox (a Tailscale node key, SSH host keys) is duplicated by every fork. If sandboxes ever join the tailnet, that state must live outside the snapshot and be regenerated per fork.
- **sbxd needs root.** ZFS and container setup need privileges, so keep the daemon's API small and reachable only over the tailnet.
- **Pick a steady OS for hosts.** On Arch, a kernel update can briefly break ZFS. Ubuntu ships ZFS with its kernel, which makes it a calmer choice for the second host.

## Open decisions and out of scope

None of these block milestone 1; each can be settled when its milestone arrives.

- [x] ~~Container runtime: Podman with `--rootfs`, `runc` directly, or `systemd-nspawn`~~ `runc`/`crun` driven directly by `sbxd`, which owns network setup. Podman would add a second source of truth and its own firewall rules; nspawn's exec story is clumsier and it isn't OCI. Revisit nspawn only if multi-container sandboxes are needed before microVMs
- [ ] API shape: HTTP and JSON or gRPC, and how exec output streams back
- [x] ~~Template manifest format: devcontainer.json subset or a small sbx-specific file~~ A small sbx-specific file: base image by digest, setup steps, limits, ports, secrets. devcontainer.json is built around Dockerfiles and features, which `sbxd` doesn't run
- [ ] Getting code in: repo baked into the template plus a `git fetch` of a requested ref at create, or pushed in per sandbox
- [x] ~~How egress isolation is enforced: host-side nftables in the forward chain, or a per-host egress proxy~~ Host-side nftables on routed veths: an allowlist in the forward chain and a closed input chain (see Sandbox boundaries). A per-host egress proxy or DNS forwarder can be added later for logging or filtering
- [ ] How ports are exposed: one forwarded port per sandbox on `sbxd`, or a `tsnet` hostname per sandbox
- [ ] Sandbox mesh: an opt-in template setting that gives each sandbox an ephemeral, tagged tailnet identity, with ACLs allowing only sandbox-to-sandbox traffic. Only when a real use case needs it, and after forking works
- [ ] Reattachable exec: a small shim per exec that holds the process and buffers its output, so an exec survives an `sbxd` restart. Deferred until something needs it
- [ ] Idle threshold for auto-pause, and what counts as activity
- [ ] Multi-container sandboxes before microVMs: nested Podman in v1, or wait for Firecracker
- [ ] Host choice beyond "has the template, least loaded"
- [ ] Capacity: how a host tracks disk it has promised versus disk actually used (clones start empty and grow), what happens when a create, fork, template build or move doesn't fit, and what to free up as the pool fills. Includes whether to cap the whole `tank/sbx` tree with a `quota`, and whether to promise more space than exists
- [ ] A name for the project

**Out of scope:** agent orchestration, session or pane UI, git and PR workflow, and which credentials an agent uses (all belong to the client on top). Also mixed machines without ZFS, other people's compute or multiple tenants, GPUs, Kubernetes, and a highly available or replicated coordinator.
