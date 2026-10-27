# Temporal in the Sandbox Project: Where It Fits and Where It Doesn't

Oct 26, 2026 · @Liam · updated Oct 27, 2026

## Summary

Use Temporal for the multi-step **procedures** that change sandboxes, and keep it out of the **hot path** and out of anything inside a sandbox. The sandboxes are ephemeral, but the operations on them are not: "move this sandbox" or "roll out a new template" can fail halfway and leave the system in a broken in-between state. Those are what durable workflows are built for.

Temporal is not the source of truth for what exists on disk. A small reconcile loop on each node compares ZFS against the desired state and cleans up whatever Temporal never knew about or lost track of. The two complement each other: Temporal does steps in order reliably; the reconciler makes reality match anyway.

## Project context

The project is a sandbox platform: clients (the CLI, the API, or agents through the MCP server) call it to create and work in sandboxes. What runs on top is out of scope. The constraints that shape this decision:

- **Templates are ZFS snapshots; sandboxes are ZFS clones.** Fork, pause and move are the core operations. Clones are near-instant to create and destroy.
- **Machines are a few nodes in a tailnet**, each with its own local ZFS pool (shared-nothing). Data moves between nodes with `zfs send`/`zfs receive`.
- **Sandboxes are ephemeral.** No strong consistency, quorum or backups. If one breaks, destroy it and reclone.
- **Templates are the only durable state**, and they should be reproducible from a recipe.
- **The main failure mode is leaks, not data loss**: clients crash or forget to clean up, and orphaned clones fill the pool.
- **Snapshots with clones can't be deleted**, so template versions have to drain before removal.
- The stack is Go, and learning Temporal is an explicit secondary goal.

## When Temporal arrives

Temporal is not needed while there is one host. In [PLAN.md](PLAN.md):

- **Milestones 1–3 (one host):** no Temporal. TTLs come from the `sbx:ttl` property and a plain reaper loop on the host.
- **Milestone 4 (second host and coordinator):** Temporal comes in alongside the coordinator. Build the move workflow first, then per-sandbox lease workflows that take over TTL expiry from the reaper. The reaper stays, narrowed to the orphan reconciler described below.

The coordinator and Temporal split the work: the coordinator's SQLite holds the template registry, the sandbox-to-host directory and placement; Temporal runs the procedures that act on them.

## Where Temporal is a good fit

Temporal earns its place wherever an operation spans several steps, several machines, or a long wait.

### Move/migrate a sandbox (saga with compensation)

Moving a sandbox is a sequence where any step can fail:

1. Snapshot the sandbox on the source node.
2. `zfs send -i template@vN sandbox@pause` from the source.
3. `zfs receive` on the target (which already has the template, so only the diff travels).
4. Update the directory to point at the target.
5. Destroy the source clone.

If step 2 or 3 dies halfway, Temporal retries the activity. If it can't succeed, a compensation step destroys the partial receive on the target and leaves the source in place. Without this, a crash mid-move can leave a sandbox on two nodes, or on none.

### Leases as durable timers

Run one workflow per sandbox. It sleeps until the sandbox's TTL; a caller renewing the lease sends a **signal** that pushes the deadline out. When the timer fires, the workflow destroys the clone. This replaces the milestone 3 reaper's TTL scan, and Temporal timers survive worker and server restarts.

### Template rollout and drain

Publishing `template@v4` is long-running: send it to every node, mark `@v4` as the default, wait until no clones of `@v3` remain, then destroy `@v3`. The wait can take hours or days. A workflow can wait that long cheaply and resume exactly where it left off.

### Host affinity through task queues

Run one Temporal worker per machine, each polling its own task queue (for example `node-gpu-box-1`). A workflow routes each ZFS activity to the queue of the machine that holds the data. This maps directly onto the shared-nothing topology.

## Where Temporal is a poor fit

Temporal adds latency, infrastructure and a second view of the world. Those costs are worth paying for procedures, not for everything.

- **The fork hot path.** A ZFS clone takes milliseconds. Each workflow step adds round-trips through the Temporal server. That's acceptable for "create sandbox" as a lifecycle operation, but the clone itself should not wait on several activity hops before the caller gets its sandbox.
- **Anything inside a sandbox.** `exec`, file reads and writes, and streaming output (the calls clients make most) belong on a direct path to the node. Routing them through workflows adds latency and bloats workflow history for no durability benefit, since the sandbox itself is disposable.
- **Being the source of truth for what exists.** Temporal only knows about operations it started. A node reboot, a manual `zfs destroy`, or a clone created outside a workflow makes its picture drift from reality. It needs a reconciler alongside it (next section).
- **Durability the sandboxes don't need.** Temporal brings its own server plus a database. For an ephemeral system that's an irony worth keeping small: `temporal server start-dev` (single binary, SQLite) or Temporal Cloud keeps it cheap for a personal project.
- **Simple "make reality match a table" jobs.** Reaping orphans or ensuring every node has the current template is a convergence problem. A reconcile loop handles those more simply and heals drift on its own.

## How the work splits

&#91;embedded content: control plane · Temporal, directory, per-node workers and reconcilers\]

Temporal starts workflows and routes each ZFS step to the worker on the node that holds the data. Each node's reconciler compares its pool with the directory and destroys anything nobody claims. Commands inside a sandbox skip both and go straight to the node.

## Who owns each need

Each project need goes to exactly one owner; the reconciler is the backstop for anything Temporal might miss.

| Need | Owner | Why |
| --- | --- | --- |
| Create / fork sandbox | Temporal (thin workflow) | Starts the lease workflow; the clone itself is one fast activity |
| Destroy on request | Temporal | Signal the sandbox's workflow so its timer and state end cleanly |
| Lease renewal and TTL expiry | Temporal (reaper before milestone 4) | Durable timer per sandbox, renewed by signal |
| Move / migrate between nodes | Temporal | Multi-step saga with compensation across two machines |
| Template rollout to all nodes | Temporal | Long-running fan-out with retries per node |
| Template drain and deletion | Temporal | Waits days for `@vN` clones to expire before destroying |
| Sandbox-to-host directory | Coordinator (SQLite) | Rebuilt from hosts; readable by both workflows and reconcilers |
| Orphaned clones (no owner) | Reconciler | Temporal can't know about clones it never created or lost track of |
| Node rejoins after reboot | Reconciler | Compares ZFS on the node with the directory and fixes the difference |
| Idle auto-pause | sbxd on the host | Local activity check; no cross-host steps |
| `exec`, files, output streaming | Neither (direct to node) | Hot path; nothing to make durable |
| Sandbox data durability | Neither | Ephemeral by design: destroy and reclone |

## Where to start

Build the move/migrate workflow first, at milestone 4. It's the feature that most needs durability, and it covers most of Temporal's core ideas in one piece of work.

1. Run `temporal server start-dev` locally and one Go worker per node, each on its own task queue.
2. Write ZFS activities as small, idempotent functions: snapshot, send, receive, destroy. Each should succeed if run twice.
3. Build the move workflow with a compensation step for a failed receive. Kill a worker mid-send to watch it recover.
4. Add the per-sandbox lease workflow: a timer, a renew signal, destroy on expiry. Retire the reaper's TTL scan.
5. Narrow the milestone 3 reaper to the per-node orphan reconciler, as a plain loop outside Temporal.

Temporal concepts this exercises: activities and retries, sagas, signals, durable timers, and task-queue routing.

### Open questions

- [ ] Does create go through a workflow, or does the API clone directly and then start the lease workflow?
- [x] ~~Where does the directory (sandbox → node) live?~~ In the coordinator's SQLite, rebuilt from hosts, so the reconciler can read it too (see [PLAN.md](PLAN.md)).
- [ ] How long should the reconciler wait before treating an unclaimed clone as an orphan, so it doesn't race an in-flight create?
- [ ] Self-hosted dev server, or Temporal Cloud once more than one person uses it?
