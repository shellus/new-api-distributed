# Distributed New API

This context describes the trusted master/edge system that distributes New API request traffic while keeping business configuration and accounting authoritative at the master.

## Nodes and execution

**Master**:
The authoritative node for users, tokens, business policy, balances, settlement, and statistics. It is not a proxy for normal edge user requests.
_Avoid_: Primary request gateway, shared database server

**Edge**:
A trusted data-plane node that authenticates and serves user requests from local policy and balance projections, then reports usage asynchronously.
_Avoid_: Slave database replica, New API cluster worker

**Complete Data Plane**:
The user-facing AI API surface shared by master and edge. An edge executes these routes locally with the same relay packages and never proxies or falls back to the master.
_Avoid_: Text-only edge boundary, master fallback

**CPA**:
The edge-local upstream execution engine that owns OAuth credentials, credential scheduling, retries, and provider execution.
_Avoid_: Billing authority, public user gateway

**Node Credential**:
The identity material by which the master trusts one edge and accepts its signed declarations and reports.
_Avoid_: User API key, CPA credential

## Synchronized business state

**Authentication Index**:
The complete set of safe token fingerprints and minimal authorization facts needed for an edge to authenticate eligible users locally.
_Avoid_: User database replica, plaintext token list

**Business Snapshot**:
A versioned projection of the user, group, model, channel, and pricing policy required by an edge to execute requests consistently with the master.
_Avoid_: Database dump, runtime status

**Edge Channel Projection**:
The locally generated channel state derived from a master channel and the standard edge CPA service convention.
_Avoid_: Manually maintained edge channel

## Accounting

**Balance Projection**:
A revisioned local projection of authoritative wallet, subscription, and token balances, reconciled with edge reservations and unsettled usage.
_Avoid_: Database replica, quota lease

**Reservation**:
A temporary local hold against one funding account and, when finite, one token account before an edge sends a request to CPA.
_Avoid_: Final charge, master reservation

**Admission Floor**:
The zero lower bound that every finite funding and token account must satisfy after a new edge reservation. It prevents edge admission from granting deliberate overdraft credit.
_Avoid_: Negative floor, settlement buffer

**Settlement Floor**:
The bounded negative balance allowed only while finalizing an already-executed request whose actual charge exceeds its reservation. It cannot authorize a new request.
_Avoid_: Admission credit, offline quota

**Settlement Circuit**:
A per-node audited safety state for excessive settlement exposure. While open, the edge cannot admit new data-plane requests, but its durable accounting remains recoverable.
_Avoid_: Node revocation, automatic retry backoff

**Usage Event**:
The durable local accounting fact produced when one edge request finishes or fails.
_Avoid_: Volatile metric, CPA usage callback

**Billing Receipt**:
The immutable accounting payload inside a usage event: frozen pricing identity, normalized usage, request-derived billing factors, and the final charge inputs needed by the shared calculator.
_Avoid_: Consume log snapshot, raw request body

**Settlement Block**:
An ordered, idempotent batch of usage events submitted by an edge and acknowledged by the master.
_Avoid_: Audit log upload, synchronous request charge

**Outbox**:
The durable edge-owned queue of usage events and settlement blocks that have not yet been acknowledged by the master.
_Avoid_: In-memory usage queue

## Addresses and configuration

**Public Address**:
The user-facing URL authoritatively declared by an authenticated edge and observed by the master for reachability and latency.
_Avoid_: Master-approved URL, CPA address

**CPA Internal Address**:
The standard container-network address used by New API Edge to reach its local CPA service.
_Avoid_: Public edge URL, per-node channel mapping
