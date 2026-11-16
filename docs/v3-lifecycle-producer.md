# IU-WS3-01: registered Worker caption/event writer

This slice uses the frozen shared `v3lifecycle` module at
`d32ea3514b1c1010391caae0348b0ff9e751f447`. It adds an explicit private
participant to the existing `jobs.Manager` event/caption publishing path.
Default v2 construction retains its existing behavior.

`LifecycleProcessIncarnation` returns owner-local freshness metadata. A caller
must obtain it from the actual Manager when constructing the explicit C06
ProducerRef and namespace; restarting a Manager cannot reinstall an old
process frame merely because a job generation returns to the same value.
The value carries no authority and does not qualify a real host/boot.

`InstallLifecycleParticipant` requires an explicit ClosureAssess profile and
limits; direct shared CC01 membership, CC02 vector, CC03 admission, an initial
revision-1 CC04 header;
one registered writer namespace; original correlation and closure references;
a local metadata witness; a separate single-attempt publisher; an explicit
release timeout; and the external effect's sink. It checks actual stream/job
generation and producer incarnation. Profiles and records are copied before
installation and snapshots are detached from private state. The CC04 record ID
uses the explicit owner prefix with its local snapshot revision; a change in
frontier/borrow state cannot reuse an earlier immutable record identity.

The initial scope is the actual event/caption writer without an active STT
session, scene/video renderer, outstanding legacy retry, or earlier writer
borrow. Such unconnected writers are refused, not omitted. Caption provider
reconfiguration is refused before secret resolution while this participant is
installed. A successor job needs a new explicit participant. This is an
internal integration unit; broad Worker/STT/scene/child integration remains a
first-release blocker, not a planned release-after feature.

The existing publish entry registers work and dispatch through
`lifecycle.ReduceAdmission` before its first product side effect, holding the
same Manager frame. The witness receives only shared metadata. The new path
uses `LifecycleEventPublisher.PublishOnce`, never the configurable legacy
client retry loop or Manager retry queue. The actual publisher call and its
borrow are tracked independently of queue length. A returned publisher call
only releases the local borrow; it does not prove the remote terminal result.

`CloseLifecycleAdmission` performs the shared cut/guard/registration reducer
sequence. A late independent callback is refused before publishing. This API
does not stop a stream, revoke a hold, grant recovery, or create a seal.
`LifecycleCompletionSnapshot` produces a shared CC04 record only after local
registration closure. It includes actual accepted/terminal frontiers, holes,
active writer obligations, effect IDs and immutable target/correlation
requirements. Pending/unknown external results remain pending/unknown under
the frozen validator. Witness ACK loss remains UNKNOWN/HELD without a retry.

The witness and single-attempt publisher are explicit integration ports. The
unit tests use synthetic witnesses/publishers; a production durable witness,
qualified provider receipt, full roster/child coverage, signed transport,
cross-process durability, and real Workload Identity are not supplied here.
No production startup or environment fallback enables the participant.
Structurally coherent provider-free fixture closure is not production
qualification or execution authority.

Test-only CI records the required roots. The tests reach the actual Manager
caption writer and cover late callback refusal, a blocked publisher with an
empty queue, restart/process and generation mismatch, witness ACK loss,
unsupported writer refusal before secret preparation, and provider ACK loss
without a retry. Fixture captions, speaker names, journal IDs, and limits are
synthetic metadata; no real user or credential is used.
