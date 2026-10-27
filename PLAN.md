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
- **Templates are root filesystems.** A container image is exported and unpacked into the template dataset, then the repo is cloned and its dependencies installed, then it is snapshotted. A sandbox is the whole machine's disk, not just a workspace folder.
- **Template recipe.** Each template is built from a recipe (Dockerfile plus a small manifest) that also declares what a sandbox of it gets: CPU and memory limits, disk quota, ports to expose, and which secrets to inject at create time. The recipe is what makes a template reproducible.
- **Runtime.** Version 1 runs each clone as a container root filesystem (Podman's `--rootfs`, or `runc` directly). Version 2 adds Firecracker microVMs on hosts with KVM, using ZFS volumes (zvols) as their disks.
- **Metadata.** ZFS user properties on each dataset, such as `sbx:template`, `sbx:template-guid`, `sbx:parent`, `sbx:created` and `sbx:ttl`.

## Sandbox boundaries

What a sandbox can reach and what it holds. These are defaults; a template can loosen them explicitly.

- **Network: isolated by default.** Egress to the internet only. No traffic to other sandboxes, the host, or anything on the tailnet (drop `100.64.0.0/10`, the host's addresses and private ranges in the sandbox's network namespace). Every host is a tailnet node, so without this rule a sandbox could reach the coordinator or other hosts. Management never needs a sandbox on the tailnet: clients go through `sbxd`, which reaches its sandboxes locally.
- **Secrets: injected, never baked.** Secrets named in the template recipe are injected at create time. Which secrets those are, and how narrowly they're scoped, is the client's concern.
- **Ports.** Ports declared in the template are reachable from your tailnet through `sbxd` (forwarded over `tsnet`), so a sandbox can run a dev server or a preview environment. Pairs naturally with branchable SQL Server databases for per-PR previews.
- **Resources.** `refquota` caps disk; cgroup limits from the template recipe cap CPU and memory. Idle sandboxes auto-pause (no exec or port traffic for a set period) and resume on the next request, in addition to TTL expiry.

## Core operations

Every operation is a thin wrapper over a few ZFS commands plus a container start or stop, so most of them take well under a second. Each one is available identically through the CLI, the API and MCP.

| Operation | What sbxd does | ZFS underneath | Cost |
| --- | --- | --- | --- |
| Build template | Unpack an image's root filesystem, clone the repo, install dependencies, snapshot | `zfs create`, then `zfs snapshot .../templates/app@v1` | Minutes, once per template version |
| Create | Clone the template snapshot, inject secrets, start a container on it | `zfs clone .../app@v1 .../sandboxes/<id>` | Clone is instant; container start dominates |
| Exec, attach and files | Run commands, stream output, read and write files over the tailnet API | None | One network round trip per call |
| Fork N ways | Freeze the sandbox, snapshot it, clone it N times, unfreeze | `zfs snapshot .../<id>@fork1`, then `zfs clone` N times | Instant; each fork pays only for its own writes |
| Pause | Stop the container, snapshot the dataset | `zfs snapshot .../<id>@pause` | Instant; keeps disk state only, not running processes |
| Resume | Start a container on the same dataset | None | Container start |
| Move to another host | Ship only the blocks changed since the template | `zfs send -i @v1 .../<id>@pause`, piped to `zfs receive` on the other host | Proportional to the sandbox's changes, not its size |
| Destroy | Stop the container, destroy the dataset; clean up unused templates | `zfs destroy` | Instant |

Freezing before a fork (Podman's `pause`, which uses the kernel's cgroup freezer) stops every process at the same instant, so the snapshot is consistent across processes. Without it, the snapshot is crash-consistent, which the SQL Server demo showed is still recoverable.

## Milestones

Each milestone ends in something you can run and measure, and stopping after milestone 4 already gives you a working, demoable system.

0. **Done: ZFS branching demo.** ZFS pool on the Arch machine, SQL Server branched and verified independent.
1. **Manual fork demo.** Build a template dataset by hand from one of your real repos with its dependencies installed. Snapshot it, clone it five times, and run the test suite in each clone with `podman run --rootfs`.
   - Done when: five sandboxes run tests in parallel and `zfs list` shows what each one cost.
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
- **Hosts are tailnet nodes.** A sandbox with default networking can reach whatever its host can. Block the tailnet and private ranges from the sandbox network namespace before anything else.
- **Forks copy everything on disk, including identities.** Anything with per-machine identity inside a sandbox (a Tailscale node key, SSH host keys) is duplicated by every fork. If sandboxes ever join the tailnet, that state must live outside the snapshot and be regenerated per fork.
- **sbxd needs root.** ZFS and container setup need privileges, so keep the daemon's API small and reachable only over the tailnet.
- **Pick a steady OS for hosts.** On Arch, a kernel update can briefly break ZFS. Ubuntu ships ZFS with its kernel, which makes it a calmer choice for the second host.

## Open decisions and out of scope

None of these block milestone 1; each can be settled when its milestone arrives.

- [ ] Container runtime: Podman with `--rootfs`, `runc` directly, or `systemd-nspawn`
- [ ] API shape: HTTP and JSON or gRPC, and how exec output streams back
- [ ] Template manifest format: devcontainer.json subset or a small sbx-specific file
- [ ] Getting code in: repo baked into the template plus a `git fetch` of a requested ref at create, or pushed in per sandbox
- [ ] How egress isolation is enforced: nftables in each sandbox's network namespace, or a per-host egress proxy
- [ ] How ports are exposed: one forwarded port per sandbox on `sbxd`, or a `tsnet` hostname per sandbox
- [ ] Sandbox mesh: an opt-in template setting that gives each sandbox an ephemeral, tagged tailnet identity, with ACLs allowing only sandbox-to-sandbox traffic. Only when a real use case needs it, and after forking works
- [ ] Idle threshold for auto-pause, and what counts as activity
- [ ] Multi-container sandboxes before microVMs: nested Podman in v1, or wait for Firecracker
- [ ] Host choice beyond "has the template, least loaded"
- [ ] A name for the project

**Out of scope:** agent orchestration, session or pane UI, git and PR workflow, and which credentials an agent uses (all belong to the client on top). Also mixed machines without ZFS, other people's compute or multiple tenants, GPUs, Kubernetes, and a highly available or replicated coordinator.
