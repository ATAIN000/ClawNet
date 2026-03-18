# Task Sync Bug Report

**Date**: 2026-03-19  
**Severity**: High — tasks never propagate to nodes that were offline during publish  
**Affected Version**: v1.0.0-beta.5 and earlier

## Symptom

Tasks created on one node are NOT discovered by other nodes that come online later, even though knowledge entries, topic messages, and swarm data all propagate correctly.

## 3-Node Test Results

### Environment

| Node | Host | Version | Peers |
|------|------|---------|-------|
| cmax | localhost | 1.0.0-beta.5 | 8 |
| bmax | bmax.chatchat.space | 1.0.0-beta.6 | 8 |
| dmax | dmax.chatchat.space | 1.0.0-beta.6 | 8 |

### Test 1: Real-time GossipSub (PASS)

1. Created task `sync-test-1773855045` on cmax
2. Waited 10 seconds
3. Checked bmax: **FOUND** — task appeared via GossipSub real-time broadcast
4. Checked dmax: **FOUND** — task appeared via GossipSub real-time broadcast

**Verdict**: Real-time gossip over `/clawnet/tasks` topic works correctly.

### Test 2: History Sync After Restart (FAIL)

1. Stopped bmax daemon (`clawnet stop`)
2. Created task `sync-while-down-test` on cmax while bmax was offline
3. Started bmax daemon back up (`clawnet start`)  
4. Waited 30+ seconds (initial sync cycle = 20s + margin)
5. Checked bmax: **NOT FOUND** — task was never synced

**Verdict**: Tasks are not included in the history sync protocol. Nodes that miss a GossipSub broadcast never receive the task.

### Existing Task Divergence

Pre-test task counts showed all 3 nodes had 50 tasks each, but with **completely different task sets** — further confirming that tasks don't sync across restarts.

## Root Cause Analysis

### How Knowledge Sync Works (working correctly)

Knowledge has **three** propagation mechanisms:

1. **Real-time GossipSub** — `publishKnowledge()` publishes to `/clawnet/knowledge` topic  
   → `handleKnowledgeSub()` receives and stores on all online peers

2. **History Sync Protocol** — `sync.go` implements `/clawnet/knowledge-sync/1.0.0`  
   → On startup (20s delay) and every 5 minutes, nodes request missing entries  
   → `syncRequest.KnowledgeSince` tells the peer what we already have  
   → Peer streams back newer entries via `syncEntry{Type: "knowledge", ...}`  
   → Uses `ListKnowledgeSince()` and `LatestKnowledgeTime()` store methods

3. **Offline Retry Queue** — `offline.go` `replayOp()` handles `case "knowledge"`  
   → Failed publishes are queued and retried every 30 seconds

### How Task Sync Works (BROKEN)

Tasks have **only one** propagation mechanism:

1. **Real-time GossipSub** — `publishTask()` publishes to `/clawnet/tasks` topic  
   → `handleTaskSub()` receives and stores

**Missing #2**: `sync.go` has NO task-related fields:
- `syncRequest` has `KnowledgeSince` and `TopicMessagesSince` but NO `TasksSince`
- `syncEntry` has types `"knowledge"`, `"topic_room"`, `"topic_message"` but NO `"task"`
- No `ListTasksSince()` or `LatestTaskTime()` store methods exist

**Missing #3**: `offline.go` `replayOp()` handles `"knowledge"`, `"dm"`, `"topic_message"` but NO `"task"`

### Impact

- A node offline during task publish **never** receives that task
- A new node joining the network receives **zero** historical tasks
- Failed task publishes (no peers connected) are **silently lost**
- Over time, each node accumulates a different random subset of tasks

## Fix Plan

### 1. Store Layer (`store/tasks.go`)

Add two new methods mirroring knowledge:
- `ListTasksSince(since string, limit int) ([]*Task, error)` — returns tasks with `created_at > since`
- `LatestTaskTime() string` — returns `MAX(created_at)` from tasks table

### 2. Sync Protocol (`daemon/sync.go`)

- Add `TasksSince string` field to `syncRequest`
- Add `Task *store.Task` field to `syncEntry`
- Stream tasks in the sync handler (alongside knowledge and topic messages)  
- Process `"task"` entries in the sync client (`syncFromPeer`)

### 3. Offline Retry (`daemon/offline.go`)

- Add `case "task"` to `replayOp()` that unmarshals and calls `publishTask()`

### 4. Queue Failed Publishes (`daemon/phase2_gossip.go`)

- In `publishTask()`, if `d.Node.Publish()` fails, queue a pending op for retry

## Post-Fix Verification (3-Node Retest)

All 3 nodes (cmax, bmax, dmax) deployed with fix and restarted.

### Test A: History Sync After Restart (PASS ✓)

1. Stopped bmax daemon
2. Created task `post-fix-sync-test` on cmax
3. Restarted bmax
4. After 25s sync cycle → **SYNCED ✓** — task appeared on bmax

### Test B: Real-time GossipSub (still works, PASS ✓)

1. Created task `bmax-origin-test` on bmax
2. After 10s → **RECEIVED ✓** on both cmax and dmax

### Test C: Previously Missing Task (PASS ✓)

- `sync-while-down-test` (created pre-fix while bmax was offline): **SYNCED ✓** after deploying fix
