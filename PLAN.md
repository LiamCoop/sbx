# ZFS agent sandboxes: build plan

Oct 26, 2026 · @Liam

## Goal and principles

Build a small system that creates, forks, pauses and moves agent sandboxes in under a second, on any machine in your tailnet that has a ZFS pool. It turns the SQL Server branching demo into a tool for agent workspaces.

- **A host is a Linux machine in the tailnet with a ZFS pool.** Nothing else is assumed, and no junkyard of mixed machines.
- **Every sandbox is a ZFS clone of a template snapshot.** Creating and forking cost almost nothing until the agent writes.
- **ZFS is the source of truth.** Sandbox metadata lives in ZFS user properties, so a host can rebuild its state from `zfs list` after a crash.
- **Measure everything.** The benchmarks are the deliverable, not just the code.
- **Trusted compute only.** Your machines, your agents.

## Architecture

With one host there is no control plane: the CLI talks to `sbxd` directly, and `zfs list` is the registry. With a second host, a small coordinator takes over the jobs that span hosts. The CLI talks to "an sbx endpoint" and sees the same API either way, so nothing changes on the client when the coordinator arrives.

&#91;embedded content: architecture · a CLI and two independent ZFS hosts\]

- **`sbx` CLI.** Runs on your laptop, and later inside the harness's Pi workers. Calls one sbx endpoint: `sbxd` on a single host, the coordinator once there are several.
- **`sbxd` host daemon.** One Go binary per host. It embeds Tailscale with `tsnet`, so it is its own node in the tailnet, and it uses Tailscale's identity lookup for auth instead of building its own.
- **Coordinator (from milestone 4).** Another `tsnet` node with SQLite, small enough to run on the desktop at first. It exposes the same API as `sbxd` and proxies `exec` to the right host. It owns the template registry, the sandbox-to-host index (including the last-seen state of offline hosts), placement, TTLs and in-flight moves.
- **Who is authoritative.** Hosts are authoritative for facts: which datasets and snapshots exist, and whether each container is running. The coordinator is authoritative only for intent and for what no single host can know. It rebuilds its sandbox index from hosts, so it never contradicts ZFS. If it is down, the global view is lost but each host still serves direct requests.
- **Identity.** A template version is identified by its snapshot's `guid`, recorded as a property from milestone 2. That is how you check that two hosts share the exact same snapshot. Sandbox IDs are globally unique (ULIDs) so per-host indexes merge without collisions.
- **ZFS layout.** Templates live at `tank/sbx/templates/<name>` with a snapshot per version (`@v1`). Sandboxes live at `tank/sbx/sandboxes/<id>`, each a clone of a template snapshot.
- **Templates are root filesystems.** A container image is exported and unpacked into the template dataset, then the repo is cloned and its dependencies installed, then it is snapshotted. A sandbox is the whole machine's disk, not just a workspace folder.
- **Runtime.** Version 1 runs each clone as a container root filesystem (Podman's `--rootfs`, or `runc` directly). Version 2 adds Firecracker microVMs on hosts with KVM, using ZFS volumes (zvols) as their disks.
- **Metadata.** ZFS user properties on each dataset, such as `sbx:template`, `sbx:template-guid`, `sbx:parent`, `sbx:created` and `sbx:ttl`.

## Core operations

Every operation is a thin wrapper over a few ZFS commands plus a container start or stop, so most of them take well under a second.

| Operation | What sbxd does | ZFS underneath | Cost |
| --- | --- | --- | --- |
| Build template | Unpack an image's root filesystem, clone the repo, install dependencies, snapshot | `zfs create`, then `zfs snapshot .../templates/app@v1` | Minutes, once per template version |
| Create | Clone the template snapshot, start a container on it | `zfs clone .../app@v1 .../sandboxes/<id>` | Clone is instant; container start dominates |
| Exec and attach | Run commands and stream output over the tailnet API | None | One network round trip per call |
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
2. **sbxd v0 on one host.** A Go daemon and CLI with create, list, exec and destroy. Templates built from a Dockerfile, metadata in ZFS properties (including the template snapshot's `guid`), ULID sandbox IDs, served on localhost only. Shape the API as the single endpoint the coordinator will later expose too.
   - Done when: `sbx create app` then `sbx exec <id> npm test` works end to end.
3. **Fork, pause, resume and cleanup.** Freeze-and-fork, pause and resume, and a reaper that destroys expired sandboxes and unused template versions.
   - Done when: you fork a running sandbox five ways mid-task, keep one, and the rest disappear on their own.
4. **Tailnet, a second host and the coordinator.** Move the API onto the tailnet with `tsnet` and Tailscale identity. Add the coordinator: template registry keyed by snapshot `guid`, a sandbox index rebuilt from hosts, simple placement (has the template, then least loaded), and moves run as one tracked operation (receive on the destination, then destroy on the source). Ship templates to new hosts with `zfs send`, and move paused sandboxes with incremental sends.
   - Done when: a sandbox paused on the desktop resumes on a cheap Hetzner box through the coordinator, the bytes sent roughly match the sandbox's changes, and `sbx list` still shows paused sandboxes while their host is offline.
5. **Benchmarks write-up.** Publish the numbers from the next section with a short explanation of where the time goes.
6. **Stretch: microVMs.** Firecracker on hosts with KVM, zvol clones as disks, then memory snapshots so forks start with processes already running.
7. **Harness integration.** The cloud agent harness uses `sbx` as a sandbox provider for its Pi workers.

## What to measure

These numbers are the portfolio piece: each one answers a question an infrastructure interviewer would ask about the design.

| Metric | How to measure | Why it matters |
| --- | --- | --- |
| Create to first command output | Time from `sbx create` to the first byte of `echo ok` | The latency an agent actually feels |
| Fork latency at 1, 5 and 20 forks | Time from fork request until every fork accepts commands | Shows forking stays flat as N grows |
| Space per sandbox | `used` from `zfs list` after a typical agent task | Proves the M + 2N cost model on real work |
| Exec round trip | Median and slowest 1 in 100 for a no-op command over the tailnet | Agents make hundreds of calls per task |
| Pause and resume time | Each step separately, with a cold and a warm host | The cost of parking idle sandboxes |
| Move cost | Bytes sent and seconds taken versus the sandbox's full size | Shows incremental sends beat full copies |
| Crash test | Kill sbxd or the host mid-snapshot, then check what survives | Where crash consistency stops being theory |

Record each with the time spent in every step (ZFS, container runtime, network), not just the total.

## Gotchas to know up front

The biggest one: build each template once and distribute it with `zfs send`, never rebuild it per host, or incremental moves stop working.

- **Incremental sends need shared history.** `zfs send -i` only works if the receiving host has the exact same snapshot, received from the sender, not an identical-looking rebuild.
- **Clones pin their template.** ZFS refuses to destroy a snapshot that still has clones. Cleanup must check each template's `clones` property first, and `zfs promote` can flip the dependency when needed.
- **Cap every sandbox.** Set `refquota` on each sandbox so a runaway agent can't fill the whole pool.
- **Snapshots hold deleted data.** Space freed inside a sandbox isn't returned while a snapshot still references it, so stale fork and pause snapshots need cleaning up too.
- **Never bake secrets into a template.** Every clone and every send carries them. Inject credentials at create time instead.
- **sbxd needs root.** ZFS and container setup need privileges, so keep the daemon's API small and reachable only over the tailnet.
- **Pick a steady OS for hosts.** On Arch, a kernel update can briefly break ZFS. Ubuntu ships ZFS with its kernel, which makes it a calmer choice for the second host.

## Open decisions and out of scope

None of these block milestone 1; each can be settled when its milestone arrives.

- [ ] Container runtime: Podman with `--rootfs`, `runc` directly, or `systemd-nspawn`
- [ ] API shape: HTTP and JSON or gRPC, and how exec output streams back
- [ ] Getting code in: repo baked into the template plus a `git fetch` at create, or pushed in per sandbox
- [ ] Credential injection for the agent's outbound requests, shared with the harness's open question
- [ ] Host choice beyond "has the template, least loaded"
- [ ] A name for the project

**Out of scope for now:** mixed machines without ZFS, other people's compute or multiple tenants, GPUs, Kubernetes, and a highly available or replicated coordinator.
