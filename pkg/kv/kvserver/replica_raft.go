// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package kvserver

import (
	"context"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/apply"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/poison"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverbase"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/liveness"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/stateloader"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/uncertainty"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"go.etcd.io/etcd/raft/v3/tracker"
)

var (
	// raftLogTruncationClearRangeThreshold is the number of entries at which Raft
	// log truncation uses a Pebble range tombstone rather than point deletes. It
	// is set high enough to avoid writing too many range tombstones to Pebble,
	// but low enough that we don't do too many point deletes either (in
	// particular, we don't want to overflow the Pebble write batch).
	//
	// In the steady state, Raft log truncation occurs when RaftLogQueueStaleSize
	// (64 KB) or RaftLogQueueStaleThreshold (100 entries) is exceeded, so
	// truncations are generally small. If followers are lagging, we let the log
	// grow to RaftLogTruncationThreshold (16 MB) before truncating.
	//
	// 100k was chosen because it is unlikely to be hit in most common cases,
	// keeping the number of range tombstones low, but will trigger when Raft logs
	// have grown abnormally large. RaftLogTruncationThreshold will typically not
	// trigger it, unless the average log entry is <= 160 bytes. The key size is
	// ~16 bytes, so Pebble point deletion batches will be bounded at ~1.6MB.
	raftLogTruncationClearRangeThreshold = uint64(util.ConstantWithMetamorphicTestRange(
		"raft-log-truncation-clearrange-threshold", 100000 /* default */, 1 /* min */, 1e6 /* max */))
)

func makeIDKey() kvserverbase.CmdIDKey {
	idKeyBuf := make([]byte, 0, kvserverbase.RaftCommandIDLen)
	idKeyBuf = encoding.EncodeUint64Ascending(idKeyBuf, uint64(rand.Int63()))
	return kvserverbase.CmdIDKey(idKeyBuf)
}

// evalAndPropose prepares the necessary pending command struct and initializes
// a client command ID if one hasn't been. A verified lease is supplied as a
// parameter if the command requires a lease; nil otherwise. It then evaluates
// the command and proposes it to Raft on success.
//
// The method accepts a concurrency guard, which it assumes responsibility for
// if it succeeds in proposing a command into Raft. If the method does not
// return an error, the guard is guaranteed to be eventually freed and the
// caller should relinquish all ownership of it. If it does return an error, the
// caller retains full ownership over the guard.
//
// evalAndPropose takes ownership of the supplied token; the caller should
// tok.Move() it into this method. It will be used to untrack the request once
// it comes out of the proposal buffer.
//
// Nothing here or below can take out a raftMu lock, since executeWriteBatch()
// is already holding readOnlyCmdMu when calling this. Locking raftMu after it
// would violate the locking order specified for Store.mu.
//
// Return values:
// - a channel which receives a response or error upon application
// - a closure used to attempt to abandon the command. When called, it unbinds
//   the command's context from its Raft proposal. The client is then free to
//   terminate execution, although it is given no guarantee that the proposal
//   won't still go on to commit and apply at some later time.
// - the proposal's ID.
// - any error obtained during the creation or proposal of the command, in
//   which case the other returned values are zero.
func (r *Replica) evalAndPropose(
	ctx context.Context,
	ba *roachpb.BatchRequest,
	g *concurrency.Guard,
	st *kvserverpb.LeaseStatus,
	ui uncertainty.Interval,
	tok TrackedRequestToken,
) (chan proposalResult, func(), kvserverbase.CmdIDKey, *StoreWriteBytes, *roachpb.Error) {
	defer tok.DoneIfNotMoved(ctx)
	idKey := makeIDKey()
	proposal, pErr := r.requestToProposal(ctx, idKey, ba, g, st, ui)
	log.Event(proposal.ctx, "evaluated request")

	// If the request hit a server-side concurrency retry error, immediately
	// propagate the error. Don't assume ownership of the concurrency guard.
	if isConcurrencyRetryError(pErr) {
		pErr = maybeAttachLease(pErr, &st.Lease)
		return nil, nil, "", nil, pErr
	} else if _, ok := pErr.GetDetail().(*roachpb.ReplicaCorruptionError); ok {
		return nil, nil, "", nil, pErr
	}

	// Attach the endCmds to the proposal and assume responsibility for
	// releasing the concurrency guard if the proposal makes it to Raft.
	proposal.ec = endCmds{repl: r, g: g, st: *st}

	// Pull out proposal channel to return. proposal.doneCh may be set to
	// nil if it is signaled in this function.
	proposalCh := proposal.doneCh

	// There are two cases where request evaluation does not lead to a Raft
	// proposal:
	// 1. proposal.command == nil indicates that the evaluation was a no-op
	//    and that no Raft command needs to be proposed.
	// 2. pErr != nil corresponds to a failed proposal - the command resulted
	//    in an error.
	if proposal.command == nil {
		if proposal.Local.RequiresRaft() {
			return nil, nil, "", nil, roachpb.NewError(errors.AssertionFailedf(
				"proposal resulting from batch %s erroneously bypassed Raft", ba))
		}
		intents := proposal.Local.DetachEncounteredIntents()
		endTxns := proposal.Local.DetachEndTxns(pErr != nil /* alwaysOnly */)
		r.handleReadWriteLocalEvalResult(ctx, *proposal.Local)

		pr := proposalResult{
			Reply:              proposal.Local.Reply,
			Err:                pErr,
			EncounteredIntents: intents,
			EndTxns:            endTxns,
		}
		proposal.finishApplication(ctx, pr)
		return proposalCh, func() {}, "", nil, nil
	}

	log.VEventf(proposal.ctx, 2,
		"proposing command to write %d new keys, %d new values, %d new intents, "+
			"write batch size=%d bytes",
		proposal.command.ReplicatedEvalResult.Delta.KeyCount,
		proposal.command.ReplicatedEvalResult.Delta.ValCount,
		proposal.command.ReplicatedEvalResult.Delta.IntentCount,
		proposal.command.WriteBatch.Size(),
	)
	// NB: if ba.AsyncConsensus is true, we will tell admission control about
	// writes that may not have happened yet. We consider this ok, since (a) the
	// typical lag in consensus is expected to be small compared to the time
	// granularity of admission control doing token and size estimation (which
	// is 15s). Also, admission control corrects for gaps in reporting.
	writeBytes := newStoreWriteBytes()
	if proposal.command.WriteBatch != nil {
		writeBytes.WriteBytes = int64(len(proposal.command.WriteBatch.Data))
	}
	if proposal.command.ReplicatedEvalResult.AddSSTable != nil {
		writeBytes.IngestedBytes = int64(len(proposal.command.ReplicatedEvalResult.AddSSTable.Data))
	}
	// If the request requested that Raft consensus be performed asynchronously,
	// return a proposal result immediately on the proposal's done channel.
	// The channel's capacity will be large enough to accommodate this.
	if ba.AsyncConsensus {
		if ets := proposal.Local.DetachEndTxns(false /* alwaysOnly */); len(ets) != 0 {
			// Disallow async consensus for commands with EndTxnIntents because
			// any !Always EndTxnIntent can't be cleaned up until after the
			// command succeeds.
			return nil, nil, "", writeBytes, roachpb.NewErrorf("cannot perform consensus asynchronously for "+
				"proposal with EndTxnIntents=%v; %v", ets, ba)
		}

		// Fork the proposal's context span so that the proposal's context
		// can outlive the original proposer's context.
		proposal.ctx, proposal.sp = tracing.ForkSpan(ctx, "async consensus")

		// Signal the proposal's response channel immediately.
		reply := *proposal.Local.Reply
		reply.Responses = append([]roachpb.ResponseUnion(nil), reply.Responses...)
		pr := proposalResult{
			Reply:              &reply,
			EncounteredIntents: proposal.Local.DetachEncounteredIntents(),
		}
		proposal.signalProposalResult(pr)

		// Continue with proposal...
	}

	// Attach information about the proposer's lease to the command, for
	// verification below raft. Lease requests are special since they are not
	// necessarily proposed under a valid lease (by necessity). Instead, they
	// reference the previous lease. Note that TransferLease also skip lease
	// checks (for technical reasons, see `TransferLease.flags`) and uses the
	// same mechanism.
	if ba.IsSingleSkipsLeaseCheckRequest() {
		// Lease-related commands have below-raft special casing and will carry the
		// lease sequence of the lease they are intending to follow.
		// The remaining requests that skip a lease check (at the time of writing
		// ProbeRequest) will assign a zero lease sequence and thus won't be able
		// to mutate state.
		var seq roachpb.LeaseSequence
		switch t := ba.Requests[0].GetInner().(type) {
		case *roachpb.RequestLeaseRequest:
			seq = t.PrevLease.Sequence
		case *roachpb.TransferLeaseRequest:
			seq = t.PrevLease.Sequence
		default:
		}
		proposal.command.ProposerLeaseSequence = seq
	} else if !st.Lease.OwnedBy(r.store.StoreID()) {
		// Perform a sanity check that the lease is owned by this replica. This must
		// have been ascertained by the callers in
		// checkExecutionCanProceedBeforeStorageSnapshot.
		log.Fatalf(ctx, "cannot propose %s on follower with remotely owned lease %s", ba, st.Lease)
	} else {
		proposal.command.ProposerLeaseSequence = st.Lease.Sequence
	}

	// Once a command is written to the raft log, it must be loaded into memory
	// and replayed on all replicas. If a command is too big, stop it here. If
	// the command is not too big, acquire an appropriate amount of quota from
	// the replica's proposal quota pool.
	//
	// TODO(tschottdorf): blocking a proposal here will leave it dangling in the
	// closed timestamp tracker for an extended period of time, which will in turn
	// prevent the node-wide closed timestamp from making progress. This is quite
	// unfortunate; we should hoist the quota pool before the reference with the
	// closed timestamp tracker is acquired. This is better anyway; right now many
	// commands can evaluate but then be blocked on quota, which has worse memory
	// behavior.
	quotaSize := uint64(proposal.command.Size())
	if maxSize := uint64(MaxCommandSize.Get(&r.store.cfg.Settings.SV)); quotaSize > maxSize {
		return nil, nil, "", nil, roachpb.NewError(errors.Errorf(
			"command is too large: %d bytes (max: %d)", quotaSize, maxSize,
		))
	}
	var err error
	proposal.quotaAlloc, err = r.maybeAcquireProposalQuota(ctx, quotaSize)
	if err != nil {
		return nil, nil, "", nil, roachpb.NewError(err)
	}
	// Make sure we clean up the proposal if we fail to insert it into the
	// proposal buffer successfully. This ensures that we always release any
	// quota that we acquire.
	defer func() {
		if pErr != nil {
			proposal.releaseQuota()
		}
	}()

	if filter := r.store.TestingKnobs().TestingProposalFilter; filter != nil {
		filterArgs := kvserverbase.ProposalFilterArgs{
			Ctx:        ctx,
			Cmd:        *proposal.command,
			QuotaAlloc: proposal.quotaAlloc,
			CmdID:      idKey,
			Req:        *ba,
		}
		if pErr = filter(filterArgs); pErr != nil {
			return nil, nil, "", nil, pErr
		}
	}

	pErr = r.propose(ctx, proposal, tok.Move(ctx))
	if pErr != nil {
		return nil, nil, "", nil, pErr
	}
	// Abandoning a proposal unbinds its context so that the proposal's client
	// is free to terminate execution. However, it does nothing to try to
	// prevent the command from succeeding. In particular, endCmds will still be
	// invoked when the command is applied. There are a handful of cases where
	// the command may not be applied (or even processed): the process crashes
	// or the local replica is removed from the range.
	abandon := func() {
		// The proposal may or may not be in the Replica's proposals map.
		// Instead of trying to look it up, simply modify the captured object
		// directly. The raftMu must be locked to modify the context of a
		// proposal because as soon as we propose a command to Raft, ownership
		// passes to the "below Raft" machinery.
		r.raftMu.Lock()
		defer r.raftMu.Unlock()
		r.mu.Lock()
		defer r.mu.Unlock()
		// TODO(radu): Should this context be created via tracer.ForkSpan?
		// We'd need to make sure the span is finished eventually.
		proposal.ctx = r.AnnotateCtx(context.TODO())
	}
	return proposalCh, abandon, idKey, writeBytes, nil
}

// propose encodes a command, starts tracking it, and proposes it to Raft.
//
// The method hands ownership of the command over to the Raft machinery. After
// the method returns, all access to the command must be performed while holding
// Replica.mu and Replica.raftMu.
//
// propose takes ownership of the supplied token; the caller should tok.Move()
// it into this method. It will be used to untrack the request once it comes out
// of the proposal buffer.
func (r *Replica) propose(
	ctx context.Context, p *ProposalData, tok TrackedRequestToken,
) (pErr *roachpb.Error) {
	defer tok.DoneIfNotMoved(ctx)

	// If an error occurs reset the command's MaxLeaseIndex to its initial value.
	// Failure to propose will propagate to the client. An invariant of this
	// package is that proposals which are finished carry a raft command with a
	// MaxLeaseIndex equal to the proposal command's max lease index.
	defer func(prev uint64) {
		if pErr != nil {
			p.command.MaxLeaseIndex = prev
		}
	}(p.command.MaxLeaseIndex)

	// Make sure the maximum lease index is unset. This field will be set in
	// propBuf.Insert and its encoded bytes will be appended to the encoding
	// buffer as a MaxLeaseFooter.
	p.command.MaxLeaseIndex = 0

	// Determine the encoding style for the Raft command.
	prefix := true
	version := kvserverbase.RaftVersionStandard
	if crt := p.command.ReplicatedEvalResult.ChangeReplicas; crt != nil {
		// EndTxnRequest with a ChangeReplicasTrigger is special because Raft
		// needs to understand it; it cannot simply be an opaque command. To
		// permit this, the command is proposed by the proposal buffer using
		// ProposeConfChange. For that reason, we also don't need a Raft command
		// prefix because the command ID is stored in a field in
		// raft.ConfChange.
		log.Infof(p.ctx, "proposing %s", crt)
		prefix = false

		// The following deals with removing a leaseholder. A voter can be removed
		// in two ways. 1) Simple (old style) where there is a reconfiguration
		// turning a voter into a LEARNER / NON-VOTER. 2) Through an intermediate
		// joint configuration, where the replica remains in the descriptor, but
		// as VOTER_{OUTGOING, DEMOTING}. When leaving the JOINT config (a second
		// Raft operation), the removed replica transitions a LEARNER / NON-VOTER.
		//
		// In case (1) the lease needs to be transferred out before a removal is
		// proposed (cooperative transfer). The code below permits leaseholder
		// removal only if entering a joint configuration (option 2 above) in which
		// the leaseholder is (any kind of) voter, and in addition, this joint config
		// should include a VOTER_INCOMING replica. In this case, the lease is
		// transferred to this new replica in maybeLeaveAtomicChangeReplicas right
		// before we exit the joint configuration.
		//
		// When the leaseholder is replaced by a new replica, transferring the
		// lease in the joint config allows transferring directly from old to new,
		// since both are active in the joint config, without going through a third
		// node or adding the new node before transferring, which might reduce
		// fault tolerance. For example, consider v1 in region1 (leaseholder), v2
		// in region2 and v3 in region3. We want to relocate v1 to a new node v4 in
		// region1. We add v4 as LEARNER. At this point we can't transfer the lease
		// to v4, so we could transfer it to v2 first, but this is likely to hurt
		// application performance. We could instead add v4 as VOTER first, and
		// then transfer lease directly to v4, but this would change the number of
		// replicas to 4, and if region1 goes down, we loose a quorum. Instead,
		// we move to a joint config where v1 (VOTER_DEMOTING_LEARNER) transfer the
		// lease to v4 (VOTER_INCOMING) directly.
		//
		// Our implementation assumes that the intention of the caller is for the
		// VOTER_INCOMING node to be the replacement replica, and hence get the
		// lease. We therefore don't dynamically select a lease target during the
		// joint config, and hand it to the VOTER_INCOMING node. This means,
		// however, that we only allow a VOTER_DEMOTING to have the lease in a
		// joint configuration, when there's also a VOTER_INCOMING node (that
		// will be used as a target for the lease transfer). Otherwise, the caller
		// is expected to shed the lease before entering a joint configuration.
		// See also https://github.com/cockroachdb/cockroach/issues/67740.
		lhRemovalAllowed := r.store.cfg.Settings.Version.IsActive(
			ctx, clusterversion.EnableLeaseHolderRemoval)
		lhDescriptor, err := r.GetReplicaDescriptor()
		if err != nil {
			return roachpb.NewError(err)
		}
		proposedDesc := p.command.ReplicatedEvalResult.State.Desc
		// This is a reconfiguration command, we make sure the proposed
		// config is legal w.r.t. the current leaseholder: we now allow the
		// leaseholder to be a VOTER_DEMOTING as long as there is a VOTER_INCOMING.
		// Otherwise, the leaseholder must be a full voter in the target config.
		// This check won't allow exiting the joint config before the lease is
		// transferred away. The previous leaseholder is a LEARNER in the target config,
		// and therefore shouldn't continue holding the lease.
		if err := roachpb.CheckCanReceiveLease(
			lhDescriptor, proposedDesc.Replicas(), lhRemovalAllowed,
		); err != nil {
			e := errors.Mark(errors.Wrapf(err, "received invalid ChangeReplicasTrigger %s to "+
				"remove self (leaseholder); lhRemovalAllowed: %v; proposed descriptor: %v", crt,
				lhRemovalAllowed, proposedDesc), errMarkInvalidReplicationChange)
			log.Errorf(p.ctx, "%v", e)
			return roachpb.NewError(e)
		}
	} else if p.command.ReplicatedEvalResult.AddSSTable != nil {
		log.VEvent(p.ctx, 4, "sideloadable proposal detected")
		version = kvserverbase.RaftVersionSideloaded
		r.store.metrics.AddSSTableProposals.Inc(1)

		if p.command.ReplicatedEvalResult.AddSSTable.Data == nil {
			return roachpb.NewErrorf("cannot sideload empty SSTable")
		}
	} else if log.V(4) {
		log.Infof(p.ctx, "proposing command %x: %s", p.idKey, p.Request.Summary())
	}

	// Create encoding buffer.
	preLen := 0
	if prefix {
		preLen = kvserverbase.RaftCommandPrefixLen
	}
	cmdLen := p.command.Size()
	// Allocate the data slice with enough capacity to eventually hold the two
	// "footers" that are filled later.
	needed := preLen + cmdLen + kvserverpb.MaxRaftCommandFooterSize()
	data := make([]byte, preLen, needed)
	// Encode prefix with command ID, if necessary.
	if prefix {
		kvserverbase.EncodeRaftCommandPrefix(data, version, p.idKey)
	}
	// Encode body of command.
	data = data[:preLen+cmdLen]
	if _, err := protoutil.MarshalTo(p.command, data[preLen:]); err != nil {
		return roachpb.NewError(err)
	}
	p.encodedCommand = data

	// Too verbose even for verbose logging, so manually enable if you want to
	// debug proposal sizes.
	if false {
		log.Infof(p.ctx, `%s: proposal: %d
  RaftCommand.ReplicatedEvalResult:          %d
  RaftCommand.ReplicatedEvalResult.Delta:    %d
  RaftCommand.WriteBatch:                    %d
`, p.Request.Summary(), cmdLen,
			p.command.ReplicatedEvalResult.Size(),
			p.command.ReplicatedEvalResult.Delta.Size(),
			p.command.WriteBatch.Size(),
		)
	}

	// Log an event if this is a large proposal. These are more likely to cause
	// blips or worse, and it's good to be able to pick them from traces.
	//
	// TODO(tschottdorf): can we mark them so lightstep can group them?
	const largeProposalEventThresholdBytes = 2 << 19 // 512kb
	if cmdLen > largeProposalEventThresholdBytes {
		log.Eventf(p.ctx, "proposal is large: %s", humanizeutil.IBytes(int64(cmdLen)))
	}

	// Insert into the proposal buffer, which passes the command to Raft to be
	// proposed. The proposal buffer assigns the command a maximum lease index
	// when it sequences it.
	//
	// NB: we must not hold r.mu while using the proposal buffer, see comment
	// on the field.
	err := r.mu.proposalBuf.Insert(ctx, p, tok.Move(ctx))
	if err != nil {
		return roachpb.NewError(err)
	}
	return nil
}

func (r *Replica) numPendingProposalsRLocked() int {
	return len(r.mu.proposals) + r.mu.proposalBuf.AllocatedIdx()
}

// hasPendingProposalsRLocked is part of the quiescer interface.
// It returns true if this node has any outstanding proposals. A client might be
// waiting for the outcome of these proposals, so we definitely don't want to
// quiesce while such proposals are in-flight.
//
// Note that this method says nothing about other node's outstanding proposals:
// if this node is the current leaseholders, previous leaseholders might have
// proposals on which they're waiting. If this node is not the current
// leaseholder, then obviously whoever is the current leaseholder might have
// pending proposals. This method is called in two places: on the current
// leaseholder when deciding whether the leaseholder should attempt to quiesce
// the range, and then on every follower to confirm that the range can indeed be
// quiesced.
func (r *Replica) hasPendingProposalsRLocked() bool {
	return r.numPendingProposalsRLocked() > 0
}

// hasPendingProposalQuotaRLocked is part of the quiescer interface. It returns
// true if there are any commands that haven't completed replicating that are
// tracked by this node's quota pool (i.e. commands that haven't been acked by
// all live replicas).
// We can't quiesce while there's outstanding quota because the respective quota
// would not be released while quiesced, and it might prevent the range from
// unquiescing (leading to deadlock). See #46699.
func (r *Replica) hasPendingProposalQuotaRLocked() bool {
	if r.mu.proposalQuota == nil {
		return true
	}
	return !r.mu.proposalQuota.Full()
}

var errRemoved = errors.New("replica removed")

// stepRaftGroup calls Step on the replica's RawNode with the provided request's
// message. Before doing so, it assures that the replica is unquiesced and ready
// to handle the request.
func (r *Replica) stepRaftGroup(req *kvserverpb.RaftMessageRequest) error {
	// We're processing an incoming raft message (from a batch that may
	// include MsgVotes), so don't campaign if we wake up our raft
	// group.
	return r.withRaftGroup(false, func(raftGroup *raft.RawNode) (bool, error) {
		// We're processing a message from another replica which means that the
		// other replica is not quiesced, so we don't need to wake the leader.
		// Note that we avoid campaigning when receiving raft messages, because
		// we expect the originator to campaign instead.
		r.maybeUnquiesceWithOptionsLocked(false /* campaignOnWake */)
		r.mu.lastUpdateTimes.update(req.FromReplica.ReplicaID, timeutil.Now())
		if req.Message.Type == raftpb.MsgSnap {
			// Occasionally a snapshot message may arrive under an outdated term,
			// which would lead to Raft discarding the snapshot. This should be
			// really rare in practice, but it does happen in tests and in particular
			// can happen to the synchronous snapshots on the learner path, which
			// will then have to wait for the raft snapshot queue to send another
			// snapshot. However, in some tests it is desirable to disable the
			// raft snapshot queue. This workaround makes that possible.
			//
			// See TestReportUnreachableRemoveRace for the test that prompted
			// this addition.
			if term := raftGroup.BasicStatus().Term; term > req.Message.Term {
				req.Message.Term = term
			}
		}
		err := raftGroup.Step(req.Message)
		if errors.Is(err, raft.ErrProposalDropped) {
			// A proposal was forwarded to this replica but we couldn't propose it.
			// Swallow the error since we don't have an effective way of signaling
			// this to the sender.
			// TODO(bdarnell): Handle ErrProposalDropped better.
			// https://github.com/cockroachdb/cockroach/issues/21849
			err = nil
		}
		return false /* unquiesceAndWakeLeader */, err
	})
}

type handleSnapshotStats struct {
	offered bool
	applied bool
}

type handleRaftReadyStats struct {
	tBegin, tEnd time.Time

	tApplicationBegin, tApplicationEnd time.Time
	apply                              applyCommittedEntriesStats

	tAppendBegin, tAppendEnd time.Time
	appendedRegularCount     int
	appendedSideloadedCount  int
	appendedSideloadedBytes  int64
	appendedRegularBytes     int64

	tPebbleCommitBegin, tPebbleCommitEnd time.Time
	pebbleBatchBytes                     int64

	tSnapBegin, tSnapEnd time.Time
	snap                 handleSnapshotStats
	sync                 bool
}

// SafeFormat implements redact.SafeFormatter
func (s handleRaftReadyStats) SafeFormat(p redact.SafePrinter, _ rune) {
	dTotal := s.tEnd.Sub(s.tBegin)
	dAppend := s.tAppendEnd.Sub(s.tAppendBegin)
	dApply := s.tApplicationEnd.Sub(s.tApplicationBegin)
	dPebble := s.tPebbleCommitEnd.Sub(s.tPebbleCommitBegin)
	dSnap := s.tSnapEnd.Sub(s.tSnapBegin)
	dUnaccounted := dTotal - dSnap - dAppend - dApply - dPebble

	{
		var sync redact.SafeString
		if s.sync {
			sync = "-sync"
		}
		p.Printf("raft ready handling: %.2fs [append=%.2fs, apply=%.2fs, commit-batch%s=%.2fs",
			dTotal.Seconds(), dAppend.Seconds(), dApply.Seconds(), sync, dPebble.Seconds())
	}
	if dSnap > 0 {
		p.Printf(", snap=%.2fs", dSnap.Seconds())
	}
	p.Printf(", other=%.2fs]", dUnaccounted.Seconds())

	p.Printf(", wrote %s",
		humanizeutil.IBytes(s.pebbleBatchBytes),
	)
	if s.sync {
		p.SafeString(" sync")
	}
	p.SafeString(" [")

	if b, n := s.appendedRegularBytes, s.appendedRegularCount; n > 0 || b > 0 {
		p.Printf("append-ent=%s (%d), ", humanizeutil.IBytes(b), n)
	}
	if b, n := s.appendedSideloadedBytes, s.appendedSideloadedCount; n > 0 || b > 0 {
		p.Printf("append-sst=%s (%d), ", humanizeutil.IBytes(b), n)
	}
	if b, n := s.apply.entriesProcessedBytes, s.apply.entriesProcessed; n > 0 || b > 0 {
		p.Printf("apply=%s (%d", humanizeutil.IBytes(b), n)
		if c := s.apply.batchesProcessed; c > 1 {
			p.Printf(" in %d batches", c)
		}
		p.SafeString(")")
	}
	p.SafeString("]")

	if n := s.apply.stateAssertions; n > 0 {
		p.Printf(", state_assertions=%d", n)
	}
	if s.snap.offered {
		if s.snap.applied {
			p.Printf(", snapshot applied")
		} else {
			p.Printf(", snapshot ignored")
		}
	}
}

func (s handleRaftReadyStats) String() string {
	return redact.StringWithoutMarkers(s)
}

// noSnap can be passed to handleRaftReady when no snapshot should be processed.
var noSnap IncomingSnapshot

// handleRaftReady processes a raft.Ready containing entries and messages that
// are ready to read, be saved to stable storage, committed, or sent to other
// peers. It takes a non-empty IncomingSnapshot to indicate that it is
// about to process a snapshot.
//
// The returned string is nonzero whenever an error is returned to give a
// non-sensitive cue as to what happened.
func (r *Replica) handleRaftReady(
	ctx context.Context, inSnap IncomingSnapshot,
) (handleRaftReadyStats, string, error) {
	r.raftMu.Lock()
	defer r.raftMu.Unlock()
	return r.handleRaftReadyRaftMuLocked(ctx, inSnap)
}

// handleRaftReadyRaftMuLocked is the same as handleRaftReady but requires that
// the replica's raftMu be held.
//
// The returned string is nonzero whenever an error is returned to give a
// non-sensitive cue as to what happened.
func (r *Replica) handleRaftReadyRaftMuLocked(
	ctx context.Context, inSnap IncomingSnapshot,
) (stats handleRaftReadyStats, _ string, _ error) {
	// handleRaftReadyRaftMuLocked is not prepared to handle context cancellation,
	// so assert that it's given a non-cancellable context.
	if ctx.Done() != nil {
		return handleRaftReadyStats{}, "", errors.AssertionFailedf(
			"handleRaftReadyRaftMuLocked cannot be called with a cancellable context")
	}

	stats = handleRaftReadyStats{
		tBegin: timeutil.Now(),
	}
	defer func() {
		// NB: we need to reference the named return parameter here. If `stats` were
		// just a local, we'd be modifying the local but not the return value.
		stats.tEnd = timeutil.Now()
	}()

	if inSnap.Desc != nil {
		stats.snap.offered = true
	}

	var hasReady bool
	var rd raft.Ready
	r.mu.Lock()
	lastIndex := r.mu.lastIndex // used for append below
	lastTerm := r.mu.lastTerm
	raftLogSize := r.mu.raftLogSize
	leaderID := r.mu.leaderID
	lastLeaderID := leaderID
	err := r.withRaftGroupLocked(true, func(raftGroup *raft.RawNode) (bool, error) {
		numFlushed, err := r.mu.proposalBuf.FlushLockedWithRaftGroup(ctx, raftGroup)
		if err != nil {
			return false, err
		}
		if hasReady = raftGroup.HasReady(); hasReady {
			rd = raftGroup.Ready()
		}
		// We unquiesce if we have a Ready (= there's work to do). We also have
		// to unquiesce if we just flushed some proposals but there isn't a
		// Ready, which can happen if the proposals got dropped (raft does this
		// if it doesn't know who the leader is). And, for extra defense in depth,
		// we also unquiesce if there are outstanding proposals.
		//
		// NB: if we had the invariant that the group can only be in quiesced
		// state if it knows the leader (state.Lead) AND we knew that raft would
		// never give us an empty ready here (i.e. the only reason to drop a
		// proposal is not knowing the leader) then numFlushed would not be
		// necessary. The latter is likely true but we don't want to rely on
		// it. The former is maybe true, but there's no easy way to enforce it.
		unquiesceAndWakeLeader := hasReady || numFlushed > 0 || len(r.mu.proposals) > 0
		return unquiesceAndWakeLeader, nil
	})
	r.mu.applyingEntries = len(rd.CommittedEntries) > 0
	pausedFollowers := r.mu.pausedFollowers
	r.mu.Unlock()
	if errors.Is(err, errRemoved) {
		// If we've been removed then just return.
		return stats, "", nil
	} else if err != nil {
		const expl = "while checking raft group for Ready"
		return stats, expl, errors.Wrap(err, expl)
	}
	if !hasReady {
		// We must update the proposal quota even if we don't have a ready.
		// Consider the case when our quota is of size 1 and two out of three
		// replicas have committed one log entry while the third is lagging
		// behind. When the third replica finally does catch up and sends
		// along a MsgAppResp, since the entry is already committed on the
		// leader replica, no Ready is emitted. But given that the third
		// replica has caught up, we can release
		// some quota back to the pool.
		r.updateProposalQuotaRaftMuLocked(ctx, lastLeaderID)
		return stats, "", nil
	}

	logRaftReady(ctx, rd)

	refreshReason := noReason
	if rd.SoftState != nil && leaderID != roachpb.ReplicaID(rd.SoftState.Lead) {
		// Refresh pending commands if the Raft leader has changed. This is usually
		// the first indication we have of a new leader on a restarted node.
		//
		// TODO(peter): Re-proposing commands when SoftState.Lead changes can lead
		// to wasteful multiple-reproposals when we later see an empty Raft command
		// indicating a newly elected leader or a conf change. Replay protection
		// prevents any corruption, so the waste is only a performance issue.
		if log.V(3) {
			log.Infof(ctx, "raft leader changed: %d -> %d", leaderID, rd.SoftState.Lead)
		}
		if !r.store.TestingKnobs().DisableRefreshReasonNewLeader {
			refreshReason = reasonNewLeader
		}
		leaderID = roachpb.ReplicaID(rd.SoftState.Lead)
	}

	if inSnap.Desc != nil {
		if !raft.IsEmptySnap(rd.Snapshot) {
			snapUUID, err := uuid.FromBytes(rd.Snapshot.Data)
			if err != nil {
				const expl = "invalid snapshot id"
				return stats, expl, errors.Wrap(err, expl)
			}
			if inSnap.SnapUUID == (uuid.UUID{}) {
				log.Fatalf(ctx, "programming error: a snapshot application was attempted outside of the streaming snapshot codepath")
			}
			if snapUUID != inSnap.SnapUUID {
				log.Fatalf(ctx, "incoming snapshot id doesn't match raft snapshot id: %s != %s", snapUUID, inSnap.SnapUUID)
			}

			// Applying this snapshot may require us to subsume one or more of our right
			// neighbors. This occurs if this replica is informed about the merges via a
			// Raft snapshot instead of a MsgApp containing the merge commits, e.g.,
			// because it went offline before the merge commits applied and did not come
			// back online until after the merge commits were truncated away.
			subsumedRepls, releaseMergeLock := r.maybeAcquireSnapshotMergeLock(ctx, inSnap)
			defer releaseMergeLock()

			stats.tSnapBegin = timeutil.Now()
			if err := r.applySnapshot(ctx, inSnap, rd.Snapshot, rd.HardState, subsumedRepls); err != nil {
				const expl = "while applying snapshot"
				return stats, expl, errors.Wrap(err, expl)
			}
			stats.tSnapEnd = timeutil.Now()
			stats.snap.applied = true

			// r.mu.lastIndex, r.mu.lastTerm and r.mu.raftLogSize were updated in
			// applySnapshot, but we also want to make sure we reflect these changes in
			// the local variables we're tracking here.
			r.mu.RLock()
			lastIndex = r.mu.lastIndex
			lastTerm = r.mu.lastTerm
			raftLogSize = r.mu.raftLogSize
			r.mu.RUnlock()

			// We refresh pending commands after applying a snapshot because this
			// replica may have been temporarily partitioned from the Raft group and
			// missed leadership changes that occurred. Suppose node A is the leader,
			// and then node C gets partitioned away from the others. Leadership passes
			// back and forth between A and B during the partition, but when the
			// partition is healed node A is leader again.
			if !r.store.TestingKnobs().DisableRefreshReasonSnapshotApplied &&
				refreshReason == noReason {
				refreshReason = reasonSnapshotApplied
			}
		}
	} else if !raft.IsEmptySnap(rd.Snapshot) {
		// If we didn't expect Raft to have a snapshot but it has one
		// regardless, that is unexpected and indicates a programming
		// error.
		err := makeNonDeterministicFailure(
			"have inSnap=nil, but raft has a snapshot %s",
			raft.DescribeSnapshot(rd.Snapshot),
		)
		return stats, getNonDeterministicFailureExplanation(err), err
	}

	// If the ready struct includes entries that have been committed, these
	// entries will be applied to the Replica's replicated state machine down
	// below, after appending new entries to the raft log and sending messages
	// to peers. However, the process of appending new entries to the raft log
	// and then applying committed entries to the state machine can take some
	// time - and these entries are already durably committed. If they have
	// clients waiting on them, we'd like to acknowledge their success as soon
	// as possible. To facilitate this, we take a quick pass over the committed
	// entries and acknowledge as many as we can trivially prove will not be
	// rejected beneath raft.
	//
	// Note that we only acknowledge up to the current last index in the Raft
	// log. The CommittedEntries slice may contain entries that are also in the
	// Entries slice (to be appended in this ready pass), and we don't want to
	// acknowledge them until they are durably in our local Raft log. This is
	// most common in single node replication groups, but it is possible when a
	// follower in a multi-node replication group is catching up after falling
	// behind. In the first case, the entries are not yet committed so
	// acknowledging them would be a lie. In the second case, the entries are
	// committed so we could acknowledge them at this point, but doing so seems
	// risky. To avoid complications in either case, we pass lastIndex for the
	// maxIndex argument to AckCommittedEntriesBeforeApplication.
	sm := r.getStateMachine()
	dec := r.getDecoder()
	appTask := apply.MakeTask(sm, dec)
	appTask.SetMaxBatchSize(r.store.TestingKnobs().MaxApplicationBatchSize)
	defer appTask.Close()
	if err := appTask.Decode(ctx, rd.CommittedEntries); err != nil {
		return stats, getNonDeterministicFailureExplanation(err), err
	}
	if err := appTask.AckCommittedEntriesBeforeApplication(ctx, lastIndex); err != nil {
		return stats, getNonDeterministicFailureExplanation(err), err
	}

	// Separate the MsgApp messages from all other Raft message types so that we
	// can take advantage of the optimization discussed in the Raft thesis under
	// the section: `10.2.1 Writing to the leader’s disk in parallel`. The
	// optimization suggests that instead of a leader writing new log entries to
	// disk before replicating them to its followers, the leader can instead
	// write the entries to disk in parallel with replicating to its followers
	// and them writing to their disks.
	//
	// Here, we invoke this optimization by:
	// 1. sending all MsgApps.
	// 2. syncing all entries and Raft state to disk.
	// 3. sending all other messages.
	//
	// Since this is all handled in handleRaftReadyRaftMuLocked, we're assured
	// that even though we may sync new entries to disk after sending them in
	// MsgApps to followers, we'll always have them synced to disk before we
	// process followers' MsgAppResps for the corresponding entries because
	// Ready processing is sequential (and because a restart of the leader would
	// prevent the MsgAppResp from being handled by it). This is important
	// because it makes sure that the leader always has all of the entries in
	// the log for its term, which is required in etcd/raft for technical
	// reasons[1].
	//
	// MsgApps are also used to inform followers of committed entries through
	// the Commit index that they contain. Due to the optimization described
	// above, a Commit index may be sent out to a follower before it is
	// persisted on the leader. This is safe because the Commit index can be
	// treated as volatile state, as is supported by raft.MustSync[2].
	// Additionally, the Commit index can never refer to entries from the
	// current Ready (due to the MsgAppResp argument above) except in
	// single-node groups, in which as a result we have to be careful to not
	// persist a Commit index without the entries its commit index might refer
	// to (see the HardState update below for details).
	//
	// [1]: the Raft thesis states that this can be made safe:
	//
	// > The leader may even commit an entry before it has been written to its
	// > own disk, if a majority of followers have written it to their disks;
	// > this is still safe.
	//
	// [2]: Raft thesis section: `3.8 Persisted state and server restarts`:
	//
	// > Other state variables are safe to lose on a restart, as they can all be
	// > recreated. The most interesting example is the commit index, which can
	// > safely be reinitialized to zero on a restart.
	//
	// Note that this will change when joint quorums are implemented, at which
	// point we have to introduce coupling between the Commit index and
	// persisted config changes, and also require some commit indexes to be
	// durably synced.
	// See:
	// https://github.com/etcd-io/etcd/issues/7625#issuecomment-489232411

	msgApps, otherMsgs := splitMsgApps(rd.Messages)
	r.traceMessageSends(msgApps, "sending msgApp")
	r.sendRaftMessagesRaftMuLocked(ctx, msgApps, pausedFollowers)

	// Use a more efficient write-only batch because we don't need to do any
	// reads from the batch. Any reads are performed on the underlying DB.
	batch := r.store.Engine().NewUnindexedBatch(false /* writeOnly */)
	defer batch.Close()

	prevLastIndex := lastIndex
	if len(rd.Entries) > 0 {
		stats.tAppendBegin = timeutil.Now()
		// All of the entries are appended to distinct keys, returning a new
		// last index.
		thinEntries, numSideloaded, sideLoadedEntriesSize, otherEntriesSize, err := r.maybeSideloadEntriesRaftMuLocked(ctx, rd.Entries)
		if err != nil {
			const expl = "during sideloading"
			return stats, expl, errors.Wrap(err, expl)
		}
		raftLogSize += sideLoadedEntriesSize
		if lastIndex, lastTerm, raftLogSize, err = r.append(
			ctx, batch, lastIndex, lastTerm, raftLogSize, thinEntries,
		); err != nil {
			const expl = "during append"
			return stats, expl, errors.Wrap(err, expl)
		}
		stats.appendedRegularCount += len(thinEntries) - numSideloaded
		stats.appendedRegularBytes += otherEntriesSize
		stats.appendedSideloadedCount += numSideloaded
		stats.appendedSideloadedBytes += sideLoadedEntriesSize
		stats.tAppendEnd = timeutil.Now()
	}

	if !raft.IsEmptyHardState(rd.HardState) {
		if !r.IsInitialized() && rd.HardState.Commit != 0 {
			log.Fatalf(ctx, "setting non-zero HardState.Commit on uninitialized replica %s. HS=%+v", r, rd.HardState)
		}
		// NB: Note that without additional safeguards, it's incorrect to write
		// the HardState before appending rd.Entries. When catching up, a follower
		// will receive Entries that are immediately Committed in the same
		// Ready. If we persist the HardState but happen to lose the Entries,
		// assertions can be tripped.
		//
		// We have both in the same batch, so there's no problem. If that ever
		// changes, we must write and sync the Entries before the HardState.
		if err := r.raftMu.stateLoader.SetHardState(ctx, batch, rd.HardState); err != nil {
			const expl = "during setHardState"
			return stats, expl, errors.Wrap(err, expl)
		}
	}
	// Synchronously commit the batch with the Raft log entries and Raft hard
	// state as we're promising not to lose this data.
	//
	// Note that the data is visible to other goroutines before it is synced to
	// disk. This is fine. The important constraints are that these syncs happen
	// before Raft messages are sent and before the call to RawNode.Advance. Our
	// regular locking is sufficient for this and if other goroutines can see the
	// data early, that's fine. In particular, snapshots are not a problem (I
	// think they're the only thing that might access log entries or HardState
	// from other goroutines). Snapshots do not include either the HardState or
	// uncommitted log entries, and even if they did include log entries that
	// were not persisted to disk, it wouldn't be a problem because raft does not
	// infer the that entries are persisted on the node that sends a snapshot.
	stats.tPebbleCommitBegin = timeutil.Now()
	stats.pebbleBatchBytes = int64(batch.Len())
	sync := rd.MustSync && !disableSyncRaftLog.Get(&r.store.cfg.Settings.SV)
	if err := batch.Commit(sync); err != nil {
		const expl = "while committing batch"
		return stats, expl, errors.Wrap(err, expl)
	}
	stats.sync = sync
	stats.tPebbleCommitEnd = timeutil.Now()
	if rd.MustSync {
		r.store.metrics.RaftLogCommitLatency.RecordValue(
			stats.tPebbleCommitEnd.Sub(stats.tPebbleCommitBegin).Nanoseconds())
	}

	if len(rd.Entries) > 0 {
		// We may have just overwritten parts of the log which contain
		// sideloaded SSTables from a previous term (and perhaps discarded some
		// entries that we didn't overwrite). Remove any such leftover on-disk
		// payloads (we can do that now because we've committed the deletion
		// just above).
		firstPurge := rd.Entries[0].Index // first new entry written
		purgeTerm := rd.Entries[0].Term - 1
		lastPurge := prevLastIndex // old end of the log, include in deletion
		purgedSize, err := maybePurgeSideloaded(ctx, r.raftMu.sideloaded, firstPurge, lastPurge, purgeTerm)
		if err != nil {
			const expl = "while purging sideloaded storage"
			return stats, expl, err
		}
		raftLogSize -= purgedSize
		if raftLogSize < 0 {
			// Might have gone negative if node was recently restarted.
			raftLogSize = 0
		}
	}

	// Update protected state - last index, last term, raft log size, and raft
	// leader ID.
	r.mu.Lock()
	r.mu.lastIndex = lastIndex
	r.mu.lastTerm = lastTerm
	r.mu.raftLogSize = raftLogSize
	var becameLeader bool
	if r.mu.leaderID != leaderID {
		r.mu.leaderID = leaderID
		// Clear the remote proposal set. Would have been nil already if not
		// previously the leader.
		becameLeader = r.mu.leaderID == r.replicaID
	}
	r.mu.Unlock()

	// When becoming the leader, proactively add the replica to the replicate
	// queue. We might have been handed leadership by a remote node which wanted
	// to remove itself from the range.
	if becameLeader && r.store.replicateQueue != nil {
		r.store.replicateQueue.MaybeAddAsync(ctx, r, r.store.Clock().NowAsClockTimestamp())
	}

	// Update raft log entry cache. We clear any older, uncommitted log entries
	// and cache the latest ones.
	r.store.raftEntryCache.Add(r.RangeID, rd.Entries, true /* truncate */)
	r.sendRaftMessagesRaftMuLocked(ctx, otherMsgs, nil /* blocked */)
	r.traceEntries(rd.CommittedEntries, "committed, before applying any entries")

	stats.tApplicationBegin = timeutil.Now()
	if len(rd.CommittedEntries) > 0 {
		err := appTask.ApplyCommittedEntries(ctx)
		stats.apply = sm.moveStats()
		if errors.Is(err, apply.ErrRemoved) {
			// We know that our replica has been removed. All future calls to
			// r.withRaftGroup() will return errRemoved so no future Ready objects
			// will be processed by this Replica.
			return stats, "", err
		} else if err != nil {
			return stats, getNonDeterministicFailureExplanation(err), err
		}
		if r.store.cfg.KVAdmissionController != nil &&
			stats.apply.followerStoreWriteBytes.numEntries > 0 {
			r.store.cfg.KVAdmissionController.FollowerStoreWriteBytes(
				r.store.StoreID(), stats.apply.followerStoreWriteBytes)
		}

		// etcd raft occasionally adds a nil entry (our own commands are never
		// empty). This happens in two situations: When a new leader is elected, and
		// when a config change is dropped due to the "one at a time" rule. In both
		// cases we may need to resubmit our pending proposals (In the former case
		// we resubmit everything because we proposed them to a former leader that
		// is no longer able to commit them. In the latter case we only need to
		// resubmit pending config changes, but it's hard to distinguish so we
		// resubmit everything anyway). We delay resubmission until after we have
		// processed the entire batch of entries.
		if stats.apply.numEmptyEntries > 0 {
			// Overwrite unconditionally since this is the most aggressive
			// reproposal mode.
			if !r.store.TestingKnobs().DisableRefreshReasonNewLeaderOrConfigChange {
				refreshReason = reasonNewLeaderOrConfigChange
			}
		}
	}
	stats.tApplicationEnd = timeutil.Now()
	applicationElapsed := stats.tApplicationEnd.Sub(stats.tApplicationBegin).Nanoseconds()
	r.store.metrics.RaftApplyCommittedLatency.RecordValue(applicationElapsed)
	r.store.metrics.RaftCommandsApplied.Inc(int64(len(rd.CommittedEntries)))
	if r.store.TestingKnobs().EnableUnconditionalRefreshesInRaftReady {
		refreshReason = reasonNewLeaderOrConfigChange
	}
	if refreshReason != noReason {
		r.mu.Lock()
		r.refreshProposalsLocked(ctx, 0 /* refreshAtDelta */, refreshReason)
		r.mu.Unlock()
	}

	// NB: if we just processed a command which removed this replica from the
	// raft group we will early return before this point. This, combined with
	// the fact that we'll refuse to process messages intended for a higher
	// replica ID ensures that our replica ID could not have changed.
	const expl = "during advance"

	r.mu.Lock()
	err = r.withRaftGroupLocked(true, func(raftGroup *raft.RawNode) (bool, error) {
		raftGroup.Advance(rd)
		if stats.apply.numConfChangeEntries > 0 {
			// If the raft leader got removed, campaign the first remaining voter.
			//
			// NB: this must be called after Advance() above since campaigning is
			// a no-op in the presence of unapplied conf changes.
			if shouldCampaignAfterConfChange(ctx, r.store.StoreID(), r.descRLocked(), raftGroup) {
				r.campaignLocked(ctx)
			}
		}

		// If the Raft group still has more to process then we immediately
		// re-enqueue it for another round of processing. This is possible if
		// the group's committed entries were paginated due to size limitations
		// and we didn't apply all of them in this pass.
		if raftGroup.HasReady() {
			r.store.enqueueRaftUpdateCheck(r.RangeID)
		}
		return true, nil
	})
	r.mu.applyingEntries = false
	r.mu.Unlock()
	if err != nil {
		return stats, expl, errors.Wrap(err, expl)
	}

	// NB: All early returns other than the one due to not having a ready
	// which also makes the below call are due to fatal errors.
	// We must also update the proposal quota when have a ready; consider the
	// case where there are two replicas and we have a quota of size 1. We
	// acquire the quota when the write gets proposed on the leader and expect it
	// to be released when the follower commits it locally. In order to do so we
	// need to have the entry 'come out of raft' and in the case of a two node
	// raft group, this only happens if hasReady == true. If we don't release
	// quota back at the end of handleRaftReadyRaftMuLocked, the next write will
	// get blocked.
	r.updateProposalQuotaRaftMuLocked(ctx, lastLeaderID)
	return stats, "", nil
}

// splitMsgApps splits the Raft message slice into two slices, one containing
// MsgApps and one containing all other message types. Each slice retains the
// relative ordering between messages in the original slice.
func splitMsgApps(msgs []raftpb.Message) (msgApps, otherMsgs []raftpb.Message) {
	splitIdx := 0
	for i, msg := range msgs {
		if msg.Type == raftpb.MsgApp {
			msgs[i], msgs[splitIdx] = msgs[splitIdx], msgs[i]
			splitIdx++
		}
	}
	return msgs[:splitIdx], msgs[splitIdx:]
}

// maybeFatalOnRaftReadyErr will fatal if err is neither nil nor
// apply.ErrRemoved.
func maybeFatalOnRaftReadyErr(ctx context.Context, expl string, err error) (removed bool) {
	switch {
	case err == nil:
		return false
	case errors.Is(err, apply.ErrRemoved):
		return true
	default:
		log.FatalfDepth(ctx, 1, "%s: %+v", redact.Safe(expl), err)
		panic("unreachable")
	}
}

// tick the Raft group, returning true if the raft group exists and should
// be queued for Ready processing; false otherwise.
func (r *Replica) tick(
	ctx context.Context, livenessMap liveness.IsLiveMap, ioThresholdMap *ioThresholdMap,
) (bool, error) {
	r.unreachablesMu.Lock()
	remotes := r.unreachablesMu.remotes
	r.unreachablesMu.remotes = nil
	r.unreachablesMu.Unlock()

	r.raftMu.Lock()
	defer r.raftMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()

	// If the raft group is uninitialized, do not initialize on tick.
	if r.mu.internalRaftGroup == nil {
		return false, nil
	}

	for remoteReplica := range remotes {
		r.mu.internalRaftGroup.ReportUnreachable(uint64(remoteReplica))
	}

	if r.mu.quiescent {
		return false, nil
	}

	r.updatePausedFollowersLocked(ctx, ioThresholdMap)

	now := r.store.Clock().NowAsClockTimestamp()
	if r.maybeQuiesceRaftMuLockedReplicaMuLocked(ctx, now, livenessMap) {
		return false, nil
	}

	r.maybeTransferRaftLeadershipToLeaseholderLocked(ctx, now)

	// For followers, we update lastUpdateTimes when we step a message from them
	// into the local Raft group. The leader won't hit that path, so we update
	// it whenever it ticks. In effect, this makes sure it always sees itself as
	// alive.
	//
	// Note that in a workload where the leader doesn't have inflight requests
	// "most of the time" (i.e. occasional writes only on this range), it's quite
	// likely that we'll never reach this line, since we'll return in the
	// maybeQuiesceRaftMuLockedReplicaMuLocked branch above.
	//
	// This is likely unintentional, and the leader should likely consider itself
	// live even when quiesced.
	if r.replicaID == r.mu.leaderID {
		r.mu.lastUpdateTimes.update(r.replicaID, timeutil.Now())
	}

	r.mu.ticks++
	preTickState := r.mu.internalRaftGroup.BasicStatus().RaftState
	r.mu.internalRaftGroup.Tick()
	postTickState := r.mu.internalRaftGroup.BasicStatus().RaftState
	if preTickState != postTickState {
		if postTickState == raft.StatePreCandidate {
			r.store.Metrics().RaftTimeoutCampaign.Inc(1)
			if k := r.store.TestingKnobs(); k != nil && k.OnRaftTimeoutCampaign != nil {
				k.OnRaftTimeoutCampaign(r.RangeID)
			}
		}
	}

	refreshAtDelta := r.store.cfg.RaftElectionTimeoutTicks
	if knob := r.store.TestingKnobs().RefreshReasonTicksPeriod; knob > 0 {
		refreshAtDelta = knob
	}
	if !r.store.TestingKnobs().DisableRefreshReasonTicks && r.mu.ticks%refreshAtDelta == 0 {
		// RaftElectionTimeoutTicks is a reasonable approximation of how long we
		// should wait before deciding that our previous proposal didn't go
		// through. Note that the combination of the above condition and passing
		// RaftElectionTimeoutTicks to refreshProposalsLocked means that commands
		// will be refreshed when they have been pending for 1 to 2 election
		// cycles.
		r.refreshProposalsLocked(ctx, refreshAtDelta, reasonTicks)
	}
	return true, nil
}

func (r *Replica) hasRaftReadyRLocked() bool {
	return r.mu.internalRaftGroup.HasReady()
}

// slowReplicationThreshold returns the threshold after which in-flight
// replicated commands should be considered "stuck" and should trip the
// per-Replica circuit breaker. The boolean indicates whether this
// mechanism is enabled; if it isn't no action should be taken.
func (r *Replica) slowReplicationThreshold(ba *roachpb.BatchRequest) (time.Duration, bool) {
	if knobs := r.store.TestingKnobs(); knobs != nil && knobs.SlowReplicationThresholdOverride != nil {
		if dur := knobs.SlowReplicationThresholdOverride(ba); dur > 0 {
			return dur, true
		}
		// Fall through.
	}
	dur := replicaCircuitBreakerSlowReplicationThreshold.Get(&r.store.cfg.Settings.SV)
	return dur, dur > 0
}

//go:generate stringer -type refreshRaftReason
type refreshRaftReason int

const (
	noReason refreshRaftReason = iota
	reasonNewLeader
	reasonNewLeaderOrConfigChange
	// A snapshot was just applied and so it may have contained commands that we
	// proposed whose proposal we still consider to be inflight. These commands
	// will never receive a response through the regular channel.
	reasonSnapshotApplied
	reasonTicks
)

// refreshProposalsLocked goes through the pending proposals, notifying
// proposers whose proposals need to be retried, and resubmitting proposals
// which were likely dropped (but may still apply at a legal Lease index) -
// ensuring that the proposer will eventually get a reply on the channel it's
// waiting on.
// mu must be held.
//
// Note that reproposals don't need to worry about checking the closed timestamp
// before reproposing, since they're reusing the original LAI.
//
// refreshAtDelta only applies for reasonTicks and specifies how old (in ticks)
// a command must be for it to be inspected; the usual value is the number of
// ticks of an election timeout (affect only proposals that have had ample time
// to apply but didn't).
func (r *Replica) refreshProposalsLocked(
	ctx context.Context, refreshAtDelta int, reason refreshRaftReason,
) {
	if refreshAtDelta != 0 && reason != reasonTicks {
		log.Fatalf(ctx, "refreshAtDelta specified for reason %s != reasonTicks", reason)
	}

	var maxSlowProposalDurationRequest *roachpb.BatchRequest
	// TODO(tbg): don't track exempt requests for tripping the breaker?
	var maxSlowProposalDuration time.Duration
	var slowProposalCount int64
	var reproposals pendingCmdSlice
	for _, p := range r.mu.proposals {
		slowReplicationThreshold, ok := r.slowReplicationThreshold(p.Request)
		// NB: ticks can be delayed, in which this detection would kick in too late
		// as well. This is unlikely to become a concern since the configured
		// durations here should be very large compared to the refresh interval, and
		// so delays shouldn't dramatically change the detection latency.
		inflightDuration := r.store.cfg.RaftTickInterval * time.Duration(r.mu.ticks-p.createdAtTicks)
		if ok && inflightDuration > slowReplicationThreshold {
			if maxSlowProposalDuration < inflightDuration {
				maxSlowProposalDuration = inflightDuration
				maxSlowProposalDurationRequest = p.Request
				slowProposalCount++
			}
		}
		switch reason {
		case reasonSnapshotApplied:
			// If we applied a snapshot, check the MaxLeaseIndexes of all
			// pending commands to see if any are now prevented from
			// applying, and if so make them return an ambiguous error. We
			// can't tell at this point (which should be rare) whether they
			// were included in the snapshot we received or not.
			//
			// NB: lease proposals have MaxLeaseIndex 0, so they are cleaned
			// up here too.
			if p.command.MaxLeaseIndex <= r.mu.state.LeaseAppliedIndex {
				r.cleanupFailedProposalLocked(p)
				log.Eventf(p.ctx, "retry proposal %x: %s", p.idKey, reason)
				p.finishApplication(ctx, proposalResult{
					Err: roachpb.NewError(
						roachpb.NewAmbiguousResultErrorf(
							"unable to determine whether command was applied via snapshot",
						),
					),
				})
			}
			continue

		case reasonTicks:
			if p.proposedAtTicks <= r.mu.ticks-refreshAtDelta {
				// The command was proposed a while ago and may have been dropped. Try it again.
				reproposals = append(reproposals, p)
			}

		default:
			// We have reason to believe that all pending proposals were
			// dropped on the floor (e.g. because of a leader election), so
			// repropose everything.
			reproposals = append(reproposals, p)
		}
	}

	r.store.metrics.SlowRaftRequests.Update(slowProposalCount)

	// If the breaker isn't tripped yet but we've detected commands that have
	// taken too long to replicate, trip the breaker now.
	//
	// NB: we still keep reproposing commands on this and subsequent ticks
	// even though this seems strictly counter-productive, except perhaps
	// for the probe's proposals. We could consider being more strict here
	// which could avoid build-up of raft log entries during outages, see
	// for example:
	// https://github.com/cockroachdb/cockroach/issues/60612
	//
	// NB: the call to Err() here also re-triggers the probe if the breaker is
	// already tripped and no probe is running, thus ensuring that even if a
	// request got added in while the probe was about to shut down, there will
	// be regular attempts at healing the breaker.
	if maxSlowProposalDuration > 0 && r.breaker.Signal().Err() == nil {
		err := errors.Errorf("have been waiting %.2fs for slow proposal %s",
			maxSlowProposalDuration.Seconds(), maxSlowProposalDurationRequest)
		log.Warningf(ctx, "%s", err)
		// NB: this is async because we're holding lots of locks here, and we want
		// to avoid having to pass all the information about the replica into the
		// breaker (since the breaker needs access to this information at will to
		// power the probe anyway). Over time, we anticipate there being multiple
		// mechanisms which trip the breaker.
		r.breaker.TripAsync(err)
	}

	if log.V(1) && len(reproposals) > 0 {
		log.Infof(ctx,
			"pending commands: reproposing %d (at %d.%d) %s",
			len(reproposals), r.mu.state.RaftAppliedIndex,
			r.mu.state.LeaseAppliedIndex, reason)
	}

	// Reproposals are those commands which we weren't able to send back to the
	// client (since we're not sure that another copy of them could apply at
	// the "correct" index). For reproposals, it's generally pretty unlikely
	// that they can make it in the right place. Reproposing in order is
	// definitely required, however.
	sort.Sort(reproposals)
	for _, p := range reproposals {
		log.Eventf(p.ctx, "re-submitting command %x to Raft: %s", p.idKey, reason)
		if err := r.mu.proposalBuf.ReinsertLocked(ctx, p); err != nil {
			r.cleanupFailedProposalLocked(p)
			p.finishApplication(ctx, proposalResult{
				Err: roachpb.NewError(roachpb.NewAmbiguousResultError(err)),
			})
		}
	}
}

func (r *Replica) poisonInflightLatches(err error) {
	r.raftMu.Lock()
	defer r.raftMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.mu.proposals {
		p.ec.poison()
		if p.ec.g.Req.PoisonPolicy == poison.Policy_Error {
			aErr := roachpb.NewAmbiguousResultError(err)
			// NB: this does not release the request's latches. It's important that
			// the latches stay in place, since the command could still apply.
			p.signalProposalResult(proposalResult{Err: roachpb.NewError(aErr)})
		}
	}
}

// maybeCoalesceHeartbeat returns true if the heartbeat was coalesced and added
// to the appropriate queue.
func (r *Replica) maybeCoalesceHeartbeat(
	ctx context.Context,
	msg raftpb.Message,
	toReplica, fromReplica roachpb.ReplicaDescriptor,
	quiesce bool,
	lagging laggingReplicaSet,
) bool {
	var hbMap map[roachpb.StoreIdent][]kvserverpb.RaftHeartbeat
	switch msg.Type {
	case raftpb.MsgHeartbeat:
		r.store.coalescedMu.Lock()
		hbMap = r.store.coalescedMu.heartbeats
	case raftpb.MsgHeartbeatResp:
		r.store.coalescedMu.Lock()
		hbMap = r.store.coalescedMu.heartbeatResponses
	default:
		return false
	}
	beat := kvserverpb.RaftHeartbeat{
		RangeID:                           r.RangeID,
		ToReplicaID:                       toReplica.ReplicaID,
		FromReplicaID:                     fromReplica.ReplicaID,
		Term:                              msg.Term,
		Commit:                            msg.Commit,
		Quiesce:                           quiesce,
		LaggingFollowersOnQuiesce:         lagging,
		LaggingFollowersOnQuiesceAccurate: quiesce,
	}
	if log.V(4) {
		log.Infof(ctx, "coalescing beat: %+v", beat)
	}
	toStore := roachpb.StoreIdent{
		StoreID: toReplica.StoreID,
		NodeID:  toReplica.NodeID,
	}
	hbMap[toStore] = append(hbMap[toStore], beat)
	r.store.coalescedMu.Unlock()
	return true
}

func (r *Replica) sendRaftMessagesRaftMuLocked(
	ctx context.Context, messages []raftpb.Message, blocked map[roachpb.ReplicaID]struct{},
) {
	var lastAppResp raftpb.Message
	for _, message := range messages {
		_, drop := blocked[roachpb.ReplicaID(message.To)]
		if drop {
			r.store.Metrics().RaftPausedFollowerDroppedMsgs.Inc(1)
		}
		switch message.Type {
		case raftpb.MsgApp:
			if util.RaceEnabled {
				// Iterate over the entries to assert that all sideloaded commands
				// are already inlined. replicaRaftStorage.Entries already performs
				// the sideload inlining for stable entries and raft.unstable always
				// contain fat entries. Since these are the only two sources that
				// raft.sendAppend gathers entries from to populate MsgApps, we
				// should never see thin entries here.
				//
				// Also assert that the log term only ever increases (most of the
				// time it stays constant, as term changes are rare), and that
				// the index increases by exactly one with each entry.
				//
				// This assertion came out of #61990.
				prevTerm := message.LogTerm // term of entry preceding the append
				prevIndex := message.Index  // index of entry preceding the append
				for j := range message.Entries {
					ent := &message.Entries[j]
					assertSideloadedRaftCommandInlined(ctx, ent)

					if prevIndex+1 != ent.Index {
						log.Fatalf(ctx,
							"index gap in outgoing MsgApp: idx %d followed by %d",
							prevIndex, ent.Index,
						)
					}
					prevIndex = ent.Index
					if prevTerm > ent.Term {
						log.Fatalf(ctx,
							"term regression in outgoing MsgApp: idx %d at term=%d "+
								"appended with logterm=%d",
							ent.Index, ent.Term, message.LogTerm,
						)
					}
					prevTerm = ent.Term
				}
			}

		case raftpb.MsgAppResp:
			// A successful (non-reject) MsgAppResp contains one piece of
			// information: the highest log index. Raft currently queues up
			// one MsgAppResp per incoming MsgApp, and we may process
			// multiple messages in one handleRaftReady call (because
			// multiple messages may arrive while we're blocked syncing to
			// disk). If we get redundant MsgAppResps, drop all but the
			// last (we've seen that too many MsgAppResps can overflow
			// message queues on the receiving side).
			//
			// Note that this reorders the chosen MsgAppResp relative to
			// other messages (including any MsgAppResps with the Reject flag),
			// but raft is fine with this reordering.
			//
			// TODO(bdarnell): Consider pushing this optimization into etcd/raft.
			// Similar optimizations may be possible for other message types,
			// although MsgAppResp is the only one that has been seen as a
			// problem in practice.
			if !message.Reject && message.Index > lastAppResp.Index {
				lastAppResp = message
				drop = true
			}
		}

		if !drop {
			r.sendRaftMessageRaftMuLocked(ctx, message)
		}
	}
	if lastAppResp.Index > 0 {
		r.sendRaftMessageRaftMuLocked(ctx, lastAppResp)
	}
}

// sendRaftMessageRaftMuLocked sends a Raft message.
func (r *Replica) sendRaftMessageRaftMuLocked(ctx context.Context, msg raftpb.Message) {
	r.mu.RLock()
	fromReplica, fromErr := r.getReplicaDescriptorByIDRLocked(roachpb.ReplicaID(msg.From), r.raftMu.lastToReplica)
	toReplica, toErr := r.getReplicaDescriptorByIDRLocked(roachpb.ReplicaID(msg.To), r.raftMu.lastFromReplica)
	var startKey roachpb.RKey
	if msg.Type == raftpb.MsgApp && r.mu.internalRaftGroup != nil {
		// When the follower is potentially an uninitialized replica waiting for
		// a split trigger, send the replica's StartKey along. See the method
		// below for more context:
		_ = maybeDropMsgApp
		// NB: this code is allocation free.
		r.mu.internalRaftGroup.WithProgress(func(id uint64, _ raft.ProgressType, pr tracker.Progress) {
			if id == msg.To && pr.State == tracker.StateProbe {
				// It is moderately expensive to attach a full key to the message, but note that
				// a probing follower will only be appended to once per heartbeat interval (i.e.
				// on the order of seconds). See:
				//
				// https://github.com/etcd-io/etcd/blob/7f450bf6967638673dd88fd4e730b01d1303d5ff/raft/progress.go#L41
				startKey = r.descRLocked().StartKey
			}
		})
	}
	r.mu.RUnlock()

	if fromErr != nil {
		log.Warningf(ctx, "failed to look up sender replica %d in r%d while sending %s: %s",
			msg.From, r.RangeID, msg.Type, fromErr)
		return
	}
	if toErr != nil {
		log.Warningf(ctx, "failed to look up recipient replica %d in r%d while sending %s: %s",
			msg.To, r.RangeID, msg.Type, toErr)
		return
	}

	// Raft-initiated snapshots are handled by the Raft snapshot queue.
	if msg.Type == raftpb.MsgSnap {
		r.store.raftSnapshotQueue.AddAsync(ctx, r, raftSnapshotPriority)
		return
	}

	if r.maybeCoalesceHeartbeat(ctx, msg, toReplica, fromReplica, false, nil) {
		return
	}

	req := newRaftMessageRequest()
	*req = kvserverpb.RaftMessageRequest{
		RangeID:       r.RangeID,
		ToReplica:     toReplica,
		FromReplica:   fromReplica,
		Message:       msg,
		RangeStartKey: startKey, // usually nil
	}
	if !r.sendRaftMessageRequest(ctx, req) {
		if err := r.withRaftGroup(true, func(raftGroup *raft.RawNode) (bool, error) {
			r.mu.droppedMessages++
			raftGroup.ReportUnreachable(msg.To)
			return true, nil
		}); err != nil && !errors.Is(err, errRemoved) {
			log.Fatalf(ctx, "%v", err)
		}
	}
}

// addUnreachableRemoteReplica adds the given remote ReplicaID to be reported
// as unreachable on the next tick.
func (r *Replica) addUnreachableRemoteReplica(remoteReplica roachpb.ReplicaID) {
	r.unreachablesMu.Lock()
	if r.unreachablesMu.remotes == nil {
		r.unreachablesMu.remotes = make(map[roachpb.ReplicaID]struct{})
	}
	r.unreachablesMu.remotes[remoteReplica] = struct{}{}
	r.unreachablesMu.Unlock()
}

// sendRaftMessageRequest sends a raft message, returning false if the message
// was dropped. It is the caller's responsibility to call ReportUnreachable on
// the Raft group.
func (r *Replica) sendRaftMessageRequest(
	ctx context.Context, req *kvserverpb.RaftMessageRequest,
) bool {
	if log.V(4) {
		log.Infof(ctx, "sending raft request %+v", req)
	}
	return r.store.cfg.Transport.SendAsync(req, r.connectionClass.get())
}

func (r *Replica) reportSnapshotStatus(ctx context.Context, to roachpb.ReplicaID, snapErr error) {
	r.raftMu.Lock()
	defer r.raftMu.Unlock()

	snapStatus := raft.SnapshotFinish
	if snapErr != nil {
		snapStatus = raft.SnapshotFailure
	}

	if err := r.withRaftGroup(true, func(raftGroup *raft.RawNode) (bool, error) {
		raftGroup.ReportSnapshot(uint64(to), snapStatus)
		return true, nil
	}); err != nil && !errors.Is(err, errRemoved) {
		log.Fatalf(ctx, "%v", err)
	}
}

type snapTruncationInfo struct {
	index          uint64
	recipientStore roachpb.StoreID
}

func (r *Replica) addSnapshotLogTruncationConstraint(
	ctx context.Context, snapUUID uuid.UUID, index uint64, recipientStore roachpb.StoreID,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addSnapshotLogTruncationConstraintLocked(ctx, snapUUID, index, recipientStore)
}

func (r *Replica) addSnapshotLogTruncationConstraintLocked(
	ctx context.Context, snapUUID uuid.UUID, index uint64, recipientStore roachpb.StoreID,
) {
	if r.mu.snapshotLogTruncationConstraints == nil {
		r.mu.snapshotLogTruncationConstraints = make(map[uuid.UUID]snapTruncationInfo)
	}
	item, ok := r.mu.snapshotLogTruncationConstraints[snapUUID]
	if ok {
		// Uh-oh, there's either a programming error (resulting in the same snapshot
		// fed into this method twice) or a UUID collision. We discard the update
		// (which is benign) but log it loudly. If the index is the same, it's
		// likely the former, otherwise the latter.
		log.Warningf(ctx, "UUID collision at %s for %+v (index %d)", snapUUID, item, index)
		return
	}

	r.mu.snapshotLogTruncationConstraints[snapUUID] = snapTruncationInfo{
		index:          index,
		recipientStore: recipientStore,
	}
}

// completeSnapshotLogTruncationConstraint marks the given snapshot as finished,
// releasing the lock on raft log truncation after a grace period.
func (r *Replica) completeSnapshotLogTruncationConstraint(snapUUID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, ok := r.mu.snapshotLogTruncationConstraints[snapUUID]
	if !ok {
		// UUID collision while adding the snapshot in originally. Nothing
		// else to do.
		return
	}
	delete(r.mu.snapshotLogTruncationConstraints, snapUUID)
	if len(r.mu.snapshotLogTruncationConstraints) == 0 {
		// Save a little bit of memory.
		r.mu.snapshotLogTruncationConstraints = nil
	}
}

// getSnapshotLogTruncationConstraints returns the minimum index of any
// currently outstanding snapshot being sent from this replica to the specified
// recipient or 0 if there isn't one. Passing 0 for recipientStore means any
// recipient.
func (r *Replica) getSnapshotLogTruncationConstraints(
	recipientStore roachpb.StoreID,
) (minSnapIndex uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.getSnapshotLogTruncationConstraintsRLocked(recipientStore)
}

func (r *Replica) getSnapshotLogTruncationConstraintsRLocked(
	recipientStore roachpb.StoreID,
) (minSnapIndex uint64) {
	for _, item := range r.mu.snapshotLogTruncationConstraints {
		if recipientStore != 0 && item.recipientStore != recipientStore {
			continue
		}
		if minSnapIndex == 0 || minSnapIndex > item.index {
			minSnapIndex = item.index
		}
	}
	return minSnapIndex
}

// hasOutstandingLearnerSnapshotInFlight returns true if there is a snapshot in
// progress from this replica to a learner replica for this range.
func (r *Replica) hasOutstandingLearnerSnapshotInFlight() bool {
	learners := r.Desc().Replicas().LearnerDescriptors()
	for _, repl := range learners {
		if r.hasOutstandingSnapshotInFlightToStore(repl.StoreID) {
			return true
		}
	}
	return false
}

// hasOutstandingSnapshotInFlightToStore returns true if there is a snapshot in
// flight from this replica to the store with the given ID.
func (r *Replica) hasOutstandingSnapshotInFlightToStore(storeID roachpb.StoreID) bool {
	return r.getSnapshotLogTruncationConstraints(storeID) > 0
}

func isRaftLeader(raftStatus *raft.Status) bool {
	return raftStatus != nil && raftStatus.SoftState.RaftState == raft.StateLeader
}

// HasRaftLeader returns true if the raft group has a raft leader currently.
func HasRaftLeader(raftStatus *raft.Status) bool {
	return raftStatus != nil && raftStatus.SoftState.Lead != 0
}

// pendingCmdSlice sorts by increasing MaxLeaseIndex.
type pendingCmdSlice []*ProposalData

func (s pendingCmdSlice) Len() int      { return len(s) }
func (s pendingCmdSlice) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s pendingCmdSlice) Less(i, j int) bool {
	return s[i].command.MaxLeaseIndex < s[j].command.MaxLeaseIndex
}

// withRaftGroupLocked calls the supplied function with the (lazily
// initialized) Raft group. The supplied function should return true for the
// unquiesceAndWakeLeader argument if the replica should be unquiesced (and the
// leader awoken). See handleRaftReady for an instance of where this value
// varies.
//
// Requires that Replica.mu is held.
//
// If this Replica is in the process of being removed this method will return
// errRemoved.
func (r *Replica) withRaftGroupLocked(
	mayCampaignOnWake bool, f func(r *raft.RawNode) (unquiesceAndWakeLeader bool, _ error),
) error {
	if r.mu.destroyStatus.Removed() {
		// Callers know to detect errRemoved as non-fatal.
		return errRemoved
	}

	if r.mu.internalRaftGroup == nil {
		ctx := r.AnnotateCtx(context.TODO())
		raftGroup, err := raft.NewRawNode(newRaftConfig(
			raft.Storage((*replicaRaftStorage)(r)),
			uint64(r.replicaID),
			r.mu.state.RaftAppliedIndex,
			r.store.cfg,
			&raftLogger{ctx: ctx},
		))
		if err != nil {
			return err
		}
		r.mu.internalRaftGroup = raftGroup

		if mayCampaignOnWake {
			r.maybeCampaignOnWakeLocked(ctx)
		}
	}

	// This wrapper function is a hack to add range IDs to stack traces
	// using the same pattern as Replica.sendWithRangeID.
	unquiesce, err := func(rangeID roachpb.RangeID, raftGroup *raft.RawNode) (bool, error) {
		return f(raftGroup)
	}(r.RangeID, r.mu.internalRaftGroup)
	if r.mu.internalRaftGroup.BasicStatus().Lead == 0 {
		// If we don't know the leader, unquiesce unconditionally. As a
		// follower, we can't wake up the leader if we don't know who that is,
		// so we should find out now before someone needs us to unquiesce.
		//
		// This situation should occur rarely or never (ever since we got
		// stricter about validating incoming Quiesce requests) but it's good
		// defense-in-depth.
		//
		// Note that maybeUnquiesceAndWakeLeaderLocked won't manage to wake up the
		// leader since it's unknown to this replica, and at the time of writing the
		// heuristics for campaigning are defensive (won't campaign if there is a
		// live leaseholder). But if we are trying to unquiesce because this
		// follower was asked to propose something, then this means that a request
		// is going to have to wait until the leader next contacts us, or, in the
		// worst case, an election timeout. This is not ideal - if a node holds a
		// live lease, we should direct the client to it immediately.
		unquiesce = true
	}
	if unquiesce {
		r.maybeUnquiesceAndWakeLeaderLocked()
	}
	return err
}

// withRaftGroup calls the supplied function with the (lazily initialized)
// Raft group. It acquires and releases the Replica lock, so r.mu must not be
// held (or acquired by the supplied function).
//
// If mayCampaignOnWake is true, the replica may initiate a raft
// election if it was previously in a dormant state. Most callers
// should set this to true, because the prevote feature minimizes the
// disruption from unnecessary elections. The exception is that we
// should not initiate an election while handling incoming raft
// messages (which may include MsgVotes from an election in progress,
// and this election would be disrupted if we started our own).
//
// If this Replica is in the process of being removed this method will return
// errRemoved.
func (r *Replica) withRaftGroup(
	mayCampaignOnWake bool, f func(r *raft.RawNode) (unquiesceAndWakeLeader bool, _ error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withRaftGroupLocked(mayCampaignOnWake, f)
}

func shouldCampaignOnWake(
	leaseStatus kvserverpb.LeaseStatus,
	storeID roachpb.StoreID,
	raftStatus raft.BasicStatus,
	livenessMap liveness.IsLiveMap,
	desc *roachpb.RangeDescriptor,
	requiresExpiringLease bool,
) bool {
	// When waking up a range, campaign unless we know that another
	// node holds a valid lease (this is most important after a split,
	// when all replicas create their raft groups at about the same
	// time, with a lease pre-assigned to one of them). Note that
	// thanks to PreVote, unnecessary campaigns are not disruptive so
	// we should err on the side of campaigining here.
	if leaseStatus.IsValid() && !leaseStatus.OwnedBy(storeID) {
		return false
	}
	// If we're already campaigning don't start a new term.
	if raftStatus.RaftState != raft.StateFollower {
		return false
	}
	// If we dont know who the leader is, then campaign.
	if raftStatus.Lead == raft.None {
		return true
	}
	// Avoid a circular dependency on liveness and skip the is leader alive check for
	// expiration based leases.
	if requiresExpiringLease {
		return false
	}
	// Determine if we think the leader is alive, if we don't have the leader
	// in the descriptor we assume it is, since it could be an indication that this
	// replica is behind.
	replDesc, ok := desc.GetReplicaDescriptorByID(roachpb.ReplicaID(raftStatus.Lead))
	if !ok {
		return false
	}
	// If we don't know about the leader in our liveness map, then we err on the side
	// of caution and dont campaign.
	livenessEntry, ok := livenessMap[replDesc.NodeID]
	if !ok {
		return false
	}
	return !livenessEntry.IsLive
}

// maybeCampaignOnWakeLocked is called when the range wakes from a
// dormant state (either the initial "raftGroup == nil" state or after
// being quiescent) and campaigns for raft leadership if appropriate.
func (r *Replica) maybeCampaignOnWakeLocked(ctx context.Context) {
	// Raft panics if a node that is not currently a member of the
	// group tries to campaign. This method should never be called
	// otherwise and in fact the Replica should never be in such a
	// state but better not take any chances. For example, if this
	// method were to be called on an uninitialized replica (which
	// has no state and thus an empty raft config), this might cause
	// problems.
	if _, currentMember := r.mu.state.Desc.GetReplicaDescriptorByID(r.replicaID); !currentMember {
		return
	}

	leaseStatus := r.leaseStatusAtRLocked(ctx, r.store.Clock().NowAsClockTimestamp())
	raftStatus := r.mu.internalRaftGroup.BasicStatus()
	livenessMap, _ := r.store.livenessMap.Load().(liveness.IsLiveMap)
	if shouldCampaignOnWake(leaseStatus, r.store.StoreID(), raftStatus, livenessMap, r.descRLocked(), r.requiresExpiringLeaseRLocked()) {
		r.campaignLocked(ctx)
	}
}

func (r *Replica) campaignLocked(ctx context.Context) {
	log.VEventf(ctx, 3, "campaigning")
	if err := r.mu.internalRaftGroup.Campaign(); err != nil {
		log.VEventf(ctx, 1, "failed to campaign: %s", err)
	}
	r.store.enqueueRaftUpdateCheck(r.RangeID)
}

// a lastUpdateTimesMap is maintained on the Raft leader to keep track of the
// last communication received from followers, which in turn informs the quota
// pool and log truncations.
type lastUpdateTimesMap map[roachpb.ReplicaID]time.Time

func (m lastUpdateTimesMap) update(replicaID roachpb.ReplicaID, now time.Time) {
	if m == nil {
		return
	}
	m[replicaID] = now
}

// updateOnUnquiesce is called when the leader unquiesces. In that case, we
// don't want live followers to appear as dead before their next message reaches
// us; to achieve that, we optimistically mark all followers that are in
// ProgressStateReplicate (or rather, were in that state when the group
// quiesced) as live as of `now`. We don't want to mark other followers as
// live as they may be down and could artificially seem alive forever assuming
// a suitable pattern of quiesce and unquiesce operations (and this in turn
// can interfere with Raft log truncations).
func (m lastUpdateTimesMap) updateOnUnquiesce(
	descs []roachpb.ReplicaDescriptor, prs map[uint64]tracker.Progress, now time.Time,
) {
	for _, desc := range descs {
		if prs[uint64(desc.ReplicaID)].State == tracker.StateReplicate {
			m.update(desc.ReplicaID, now)
		}
	}
}

// updateOnBecomeLeader is similar to updateOnUnquiesce, but is called when the
// replica becomes the Raft leader. It updates all followers irrespective of
// their Raft state, for the Raft state is not yet populated by the time this
// callback is invoked. Raft leadership is usually stable, so there is no danger
// of artificially keeping down followers alive, though if it started
// flip-flopping at a <10s cadence there would be a risk of that happening.
func (m lastUpdateTimesMap) updateOnBecomeLeader(descs []roachpb.ReplicaDescriptor, now time.Time) {
	for _, desc := range descs {
		m.update(desc.ReplicaID, now)
	}
}

// isFollowerActiveSince returns whether the specified follower has made
// communication with the leader recently (since threshold).
func (m lastUpdateTimesMap) isFollowerActiveSince(
	ctx context.Context, replicaID roachpb.ReplicaID, now time.Time, threshold time.Duration,
) bool {
	lastUpdateTime, ok := m[replicaID]
	if !ok {
		// If the follower has no entry in lastUpdateTimes, it has not been
		// updated since r became the leader (at which point all then-existing
		// replicas were updated).
		return false
	}
	return now.Sub(lastUpdateTime) <= threshold
}

// maybeAcquireSnapshotMergeLock checks whether the incoming snapshot subsumes
// any replicas and, if so, locks them for subsumption. See acquireMergeLock
// for details about the lock itself.
func (r *Replica) maybeAcquireSnapshotMergeLock(
	ctx context.Context, inSnap IncomingSnapshot,
) (subsumedRepls []*Replica, releaseMergeLock func()) {
	// Any replicas that overlap with the bounds of the incoming snapshot are ours
	// to subsume; further, the end of the last overlapping replica will exactly
	// align with the end of the snapshot. How are we guaranteed this? Each merge
	// could not have committed unless this store had an up-to-date replica of the
	// RHS at the time of the merge. Nothing could have removed that RHS replica,
	// as the replica GC queue cannot GC a replica unless it can prove its
	// left-hand neighbor has no pending merges to apply. And that RHS replica
	// could not have been further split or merged, as it never processes another
	// command after the merge commits.
	endKey := r.Desc().EndKey
	if endKey == nil {
		// The existing replica is unitialized, in which case we've already
		// installed a placeholder for snapshot's keyspace. No merge lock needed.
		return nil, func() {}
	}
	for endKey.Less(inSnap.Desc.EndKey) {
		sRepl := r.store.LookupReplica(endKey)
		if sRepl == nil || !endKey.Equal(sRepl.Desc().StartKey) {
			log.Fatalf(ctx, "snapshot widens existing replica, but no replica exists for subsumed key %s", endKey)
		}
		sRepl.raftMu.Lock()
		subsumedRepls = append(subsumedRepls, sRepl)
		endKey = sRepl.Desc().EndKey
	}
	return subsumedRepls, func() {
		for _, sr := range subsumedRepls {
			sr.raftMu.Unlock()
		}
	}
}

// maybeAcquireSplitMergeLock examines the given raftCmd (which need
// not be applied yet) and acquires the split or merge lock if
// necessary (in addition to other preparation). It returns a function
// which will release any lock acquired (or nil).
//
// After this method returns successfully the RHS of the split or merge
// is guaranteed to exist in the Store using GetReplica().
func (r *Replica) maybeAcquireSplitMergeLock(
	ctx context.Context, raftCmd kvserverpb.RaftCommand,
) (func(), error) {
	if split := raftCmd.ReplicatedEvalResult.Split; split != nil {
		return r.acquireSplitLock(ctx, &split.SplitTrigger)
	} else if merge := raftCmd.ReplicatedEvalResult.Merge; merge != nil {
		return r.acquireMergeLock(ctx, &merge.MergeTrigger)
	}
	return nil, nil
}

func (r *Replica) acquireSplitLock(
	ctx context.Context, split *roachpb.SplitTrigger,
) (func(), error) {
	rightReplDesc, _ := split.RightDesc.GetReplicaDescriptor(r.StoreID())
	rightRepl, _, err := r.store.getOrCreateReplica(
		ctx, split.RightDesc.RangeID, rightReplDesc.ReplicaID, nil, /* creatingReplica */
	)
	// If getOrCreateReplica returns RaftGroupDeletedError we know that the RHS
	// has already been removed. This case is handled properly in splitPostApply.
	if errors.HasType(err, (*roachpb.RaftGroupDeletedError)(nil)) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	// The right hand side of a split is always uninitialized since
	// the left hand side blocks snapshots to it, and a snapshot is
	// required to initialize it (if the split trigger doesn't - and
	// this code here is part of the split trigger).
	if rightRepl.IsInitialized() {
		return nil, errors.Errorf("RHS of split %s / %s already initialized before split application",
			&split.LeftDesc, &split.RightDesc)
	}
	return rightRepl.raftMu.Unlock, nil
}

func (r *Replica) acquireMergeLock(
	ctx context.Context, merge *roachpb.MergeTrigger,
) (func(), error) {
	// The merge lock is the right-hand replica's raftMu. The right-hand replica
	// is required to exist on this store at the merge implied replica ID.
	// Otherwise, an incoming snapshot could create the right-hand replica before
	// the merge trigger has a chance to widen the left-hand replica's end key.
	// The merge trigger would then fatal the node upon realizing the right-hand
	// replica already exists. With a right-hand replica in place, any snapshots
	// for the right-hand range will block on raftMu, waiting for the merge to
	// complete, after which the replica will realize it has been destroyed and
	// reject the snapshot.
	rightReplDesc, _ := merge.RightDesc.GetReplicaDescriptor(r.StoreID())
	rightRepl, _, err := r.store.getOrCreateReplica(
		ctx, merge.RightDesc.RangeID, rightReplDesc.ReplicaID, nil, /* creatingReplica */
	)
	if err != nil {
		return nil, err
	}
	rightDesc := rightRepl.Desc()
	if !rightDesc.StartKey.Equal(merge.RightDesc.StartKey) || !rightDesc.EndKey.Equal(merge.RightDesc.EndKey) {
		return nil, errors.Errorf("RHS of merge %s <- %s not present on store; found %s in place of the RHS",
			&merge.LeftDesc, &merge.RightDesc, rightDesc)
	}
	return rightRepl.raftMu.Unlock, nil
}

// handleTruncatedStateBelowRaftPreApply is called before applying a Raft
// command that updates the truncated state.
//
// The truncated state of a replica determines where its Raft log starts (by
// giving the last index that was already deleted). It's unreplicated -- it can
// differ between replicas at the same applied index. This divergence occurs
// primarily occurs through snapshots that contain no log entries; the truncated
// index in the snapshot is set to equal the applied index it was generated
// from. The truncation itself then is a purely replicated side effect.
//
// Updates to the HardState are sent out by a leaseholder truncating the log
// based on its local knowledge. For example, the leader might have a log
// 10..100 and truncates to 50, and will send out a TruncatedState with Index 50
// to that effect. However, some replicas may not even have log entries that
// old and must make sure to ignore this update to the truncated state, as it
// would otherwise clobber their "newer" truncated state. The truncated state
// provided by the leader then is merely a suggested one -- we could ignore it
// and still be correct.
//
// We also rely on log truncations happening in the apply loop -- this makes
// sure that a truncation does not remove entries to be applied that we haven't
// yet. Since a truncation only ever removes committed log indexes, and after
// choosing the index gets proposed, the truncation command itself will be
// assigned an index higher than the one it could possibly remove. By the time
// the truncation itself is handled, the state machine will have applied all
// entries the truncation could possibly affect.
//
// The returned boolean tells the caller whether to apply the truncated state's
// side effects, which means replacing the in-memory TruncatedState and applying
// the associated RaftLogDelta. It is usually expected to be true, but may not
// be for the first truncation after on a replica that recently received a
// snapshot.
func handleTruncatedStateBelowRaftPreApply(
	ctx context.Context,
	currentTruncatedState, suggestedTruncatedState *roachpb.RaftTruncatedState,
	loader stateloader.StateLoader,
	readWriter storage.ReadWriter,
) (_apply bool, _ error) {
	if suggestedTruncatedState.Index <= currentTruncatedState.Index {
		// The suggested truncated state moves us backwards; instruct the
		// caller to not update the in-memory state.
		return false, nil
	}

	// Truncate the Raft log from the entry after the previous
	// truncation index to the new truncation index. This is performed
	// atomically with the raft command application so that the
	// TruncatedState index is always consistent with the state of the
	// Raft log itself.
	prefixBuf := &loader.RangeIDPrefixBuf
	numTruncatedEntries := suggestedTruncatedState.Index - currentTruncatedState.Index
	if numTruncatedEntries >= raftLogTruncationClearRangeThreshold {
		start := prefixBuf.RaftLogKey(currentTruncatedState.Index + 1).Clone()
		end := prefixBuf.RaftLogKey(suggestedTruncatedState.Index + 1).Clone() // end is exclusive
		if err := readWriter.ClearRawRange(start, end, true, false); err != nil {
			return false, errors.Wrapf(err,
				"unable to clear truncated Raft entries for %+v between indexes %d-%d",
				suggestedTruncatedState, currentTruncatedState.Index+1, suggestedTruncatedState.Index+1)
		}
	} else {
		// NB: RangeIDPrefixBufs have sufficient capacity (32 bytes) to
		// avoid allocating when constructing Raft log keys (16 bytes).
		prefix := prefixBuf.RaftLogPrefix()
		for idx := currentTruncatedState.Index + 1; idx <= suggestedTruncatedState.Index; idx++ {
			if err := readWriter.ClearUnversioned(keys.RaftLogKeyFromPrefix(prefix, idx)); err != nil {
				return false, errors.Wrapf(err, "unable to clear truncated Raft entries for %+v at index %d",
					suggestedTruncatedState, idx)
			}
		}
	}

	// The suggested truncated state moves us forward; apply it and tell
	// the caller as much.
	if err := storage.MVCCPutProto(
		ctx,
		readWriter,
		nil, /* ms */
		prefixBuf.RaftTruncatedStateKey(),
		hlc.Timestamp{},
		hlc.ClockTimestamp{},
		nil, /* txn */
		suggestedTruncatedState,
	); err != nil {
		return false, errors.Wrap(err, "unable to write RaftTruncatedState")
	}

	return true, nil
}

// ComputeRaftLogSize computes the size (in bytes) of the Raft log from the
// storage engine. This will iterate over the Raft log and sideloaded files, so
// depending on the size of these it can be mildly to extremely expensive and
// thus should not be called frequently.
//
// The sideloaded storage may be nil, in which case it is treated as empty.
func ComputeRaftLogSize(
	ctx context.Context, rangeID roachpb.RangeID, reader storage.Reader, sideloaded SideloadStorage,
) (int64, error) {
	prefix := keys.RaftLogPrefix(rangeID)
	prefixEnd := prefix.PrefixEnd()
	ms, err := storage.ComputeStats(reader, prefix, prefixEnd, 0 /* nowNanos */)
	if err != nil {
		return 0, err
	}
	var totalSideloaded int64
	if sideloaded != nil {
		var err error
		// The remaining bytes if one were to truncate [0, 0) gives us the total
		// number of bytes in sideloaded files.
		_, totalSideloaded, err = sideloaded.BytesIfTruncatedFromTo(ctx, 0, 0)
		if err != nil {
			return 0, err
		}
	}
	return ms.SysBytes + totalSideloaded, nil
}

func shouldCampaignAfterConfChange(
	ctx context.Context,
	storeID roachpb.StoreID,
	desc *roachpb.RangeDescriptor,
	raftGroup *raft.RawNode,
) bool {
	// If a config change was carried out, it's possible that the Raft
	// leader was removed. Verify that, and if so, campaign if we are
	// the first remaining voter replica. Without this, the range will
	// be leaderless (and thus unavailable) for a few seconds.
	//
	// We can't (or rather shouldn't) campaign on all remaining voters
	// because that can lead to a stalemate. For example, three voters
	// may all make it through PreVote and then reject each other.
	st := raftGroup.BasicStatus()
	if st.Lead == 0 {
		// Leader unknown. This isn't what we expect in steady state, so we
		// don't do anything.
		return false
	}
	if !desc.IsInitialized() {
		// We don't have an initialized, so we can't figure out who is supposed
		// to campaign. It's possible that it's us and we're waiting for the
		// initial snapshot, but it's hard to tell. Don't do anything.
		return false
	}
	// If the leader is no longer in the descriptor but we are the first voter,
	// campaign.
	_, leaderStillThere := desc.GetReplicaDescriptorByID(roachpb.ReplicaID(st.Lead))
	if !leaderStillThere && storeID == desc.Replicas().VoterDescriptors()[0].StoreID {
		log.VEventf(ctx, 3, "leader got removed by conf change")
		return true
	}
	return false
}

func getNonDeterministicFailureExplanation(err error) string {
	if nd := (*nonDeterministicFailure)(nil); errors.As(err, &nd) {
		return nd.safeExpl
	}
	return "???"
}

// printRaftTail pretty-prints the tail of the log and returns it as a string,
// with the same format as `cockroach debug raft-log`. The entries are printed
// from newest to oldest. maxEntries and maxCharsPerEntry control the size of
// the output.
//
// If an error is returned, it's possible that a string with some entries is
// still returned.
func (r *Replica) printRaftTail(
	ctx context.Context, maxEntries, maxCharsPerEntry int,
) (string, error) {
	start := keys.RaftLogPrefix(r.RangeID)
	end := keys.RaftLogPrefix(r.RangeID).PrefixEnd()

	// NB: raft log does not have intents.
	it := r.Engine().NewEngineIterator(storage.IterOptions{LowerBound: start, UpperBound: end})
	valid, err := it.SeekEngineKeyLT(storage.EngineKey{Key: end})
	if err != nil {
		return "", err
	}
	if !valid {
		return "", errors.AssertionFailedf("iterator invalid but no error")
	}

	var sb strings.Builder
	for i := 0; i < maxEntries; i++ {
		key, err := it.EngineKey()
		if err != nil {
			return sb.String(), err
		}
		mvccKey, err := key.ToMVCCKey()
		if err != nil {
			return sb.String(), err
		}
		kv := storage.MVCCKeyValue{
			Key:   mvccKey,
			Value: it.Value(),
		}
		sb.WriteString(truncateEntryString(SprintMVCCKeyValue(kv, true /* printKey */), 2000))
		sb.WriteRune('\n')

		valid, err := it.PrevEngineKey()
		if err != nil {
			return sb.String(), err
		}
		if !valid {
			// We've finished the log.
			break
		}
	}
	return sb.String(), nil
}

func truncateEntryString(s string, maxChars int) string {
	res := s
	if len(s) > maxChars {
		if maxChars > 3 {
			maxChars -= 3
		}
		res = s[0:maxChars] + "..."
	}
	return res
}
