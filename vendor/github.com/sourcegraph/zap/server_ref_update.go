package zap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	logpkg "github.com/go-kit/kit/log"
	level "github.com/go-kit/kit/log/experimental_level"
	"github.com/neelance/parallel"
	"github.com/sourcegraph/jsonrpc2"
	"github.com/sourcegraph/zap/ot"
	"github.com/sourcegraph/zap/server/refdb"
	"github.com/sourcegraph/zap/ws"
)

func (s *Server) handleRefUpdateFromUpstream(ctx context.Context, log *logpkg.Context, params RefUpdateDownstreamParams, endpoint string) error {
	// s.recvMu.Lock()
	// defer s.recvMu.Unlock()

	if err := params.validate(); err != nil {
		return &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInvalidParams,
			Message: "invalid params for ref update from upstream: " + err.Error(),
		}
	}

	// Find the local repo.
	repo, localRepoName, remote := s.findLocalRepo(params.RefIdentifier.Repo, endpoint)
	if repo == nil {
		return &jsonrpc2.Error{
			Code:    int64(ErrorCodeRepoNotExists),
			Message: fmt.Sprintf("ref update from upstream failed because no local repo is tracking remote repo %q at endpoint %q", params.RefIdentifier.Repo, endpoint),
		}
	}
	params.RefIdentifier.Repo = localRepoName

	// Update the remote tracking branch.
	remoteTrackingParams := params
	remoteTrackingParams.Ref = remoteTrackingRef(remote, params.RefIdentifier.Ref)
	remoteTrackingParams.Ack = false
	if err := s.updateRemoteTrackingRef(ctx, log, repo, remoteTrackingParams); err != nil {
		return err
	}

	// Update the local tracking branch for this upstream branch, if any.
	repo.mu.Lock()
	refConfig, ok := repo.config.Refs[params.RefIdentifier.Ref]
	repo.mu.Unlock()
	if ok && refConfig.Upstream == remote {
		ref := repo.refdb.Lookup(params.RefIdentifier.Ref)
		if ref == nil {
			level.Warn(log).Log("upstream-configured-for-nonexistent-ref", params.RefIdentifier.Ref)
		} else {
			if err := s.updateLocalTrackingRefAfterUpstreamUpdate(ctx, log, repo, *ref, params, refConfig); err != nil {
				return err
			}
		}
	} else {
		level.Debug(log).Log("no-local-ref-downstream-of", params.RefIdentifier.Ref)
	}
	return nil
}

func (s *Server) updateRemoteTrackingRef(ctx context.Context, log *logpkg.Context, repo *serverRepo, params RefUpdateDownstreamParams) error {
	log = log.With("update-remote-tracking-ref", params.RefIdentifier.Ref, "params", params)
	level.Info(log).Log()

	// When we connect to and repo/watch on a new remote server in
	// quick succession, if a branch is configured to overwrite, this
	// method will be called twice in quick succession. This can cause
	// an error of the form "refdb write: wrong old value for ref
	// refs/remotes/origin/master@sqs: actual != nil && expected ==
	// nil".
	//
	// TODO(sqs) HACK: mitigate this by locking; we should come up
	// with a better solution.
	s.updateRemoteTrackingRefMu.Lock()
	defer s.updateRemoteTrackingRefMu.Unlock()

	timer := time.AfterFunc(1*time.Second, func() {
		level.Warn(log).Log("delay", "taking a long time, possible deadlock")
	})
	defer timer.Stop()

	refClosure := repo.refdb.TransitiveClosureRefs(params.RefIdentifier.Ref)

	ref := repo.refdb.Lookup(params.RefIdentifier.Ref)
	if params.Ack {
		// Nothing to do.
	} else if params.Delete {
		// Delete ref.
		if ref != nil {
			if err := repo.refdb.Delete(params.RefIdentifier.Ref, *ref, refdb.RefLogEntry{}); err != nil {
				return err
			}
		} else {
			level.Warn(log).Log("delete-of-nonexistent-ref", "")
		}
	} else {
		var oldRef *refdb.Ref
		if ref != nil {
			tmp := *ref
			oldRef = &tmp
		}

		if params.State != nil {
			ref = &refdb.Ref{
				Name: params.RefIdentifier.Ref,
				Object: serverRef{
					gitBase:   params.State.GitBase,
					gitBranch: params.State.GitBranch,
					ot:        &ws.Proxy{},
				},
			}

			if oldRef == nil {
				// If this ref never existed before, then it wouldn't yet
				// exist in its own refClosure, so we must add it.
				refClosure = append(refClosure, *ref)
			}

			for _, op := range params.State.History {
				// OK to discard the RecvFromUpstream transformed op
				// return value because we know otHandler's history
				// started out empty (because we just created it).
				if _, err := ref.Object.(serverRef).ot.RecvFromUpstream(log, op); err != nil {
					return err
				}
			}
		} else if params.Op != nil {
			if ref == nil {
				return &jsonrpc2.Error{
					Code:    int64(ErrorCodeRefNotExists),
					Message: fmt.Sprintf("received upstream op for remote tracking branch %q but the branch does not exist", params.RefIdentifier.Ref),
				}
			}
			if err := compareRefBaseInfo(*params.Current, ref.Object.(serverRef)); err != nil {
				return &jsonrpc2.Error{
					Code:    int64(ErrorCodeRefConflict),
					Message: fmt.Sprintf("received upstream op for remote tracking branch %q with conflicting ref state: %s", params.RefIdentifier.Ref, err),
				}
			}
			xop, err := ref.Object.(serverRef).ot.RecvFromUpstream(log, *params.Op)
			if err != nil {
				return err
			}
			if op, xop := ot.NormalizeWorkspaceOp(*params.Op), ot.NormalizeWorkspaceOp(xop); !reflect.DeepEqual(op, xop) {
				panic(fmt.Sprintf("expected remote tracking ref %q to not transform ops since it only receives them from a single source, but got %v != %v", ref.Name, op, xop))
			}
		}

		if err := repo.refdb.Write(*ref, true, oldRef, refdb.RefLogEntry{}); err != nil {
			return err
		}
	}

	return s.broadcastRefUpdateDownstream(ctx, log, params.RefIdentifier.Repo, withoutSymbolicRefs(refClosure), nil, params)
}

func (s *Server) updateLocalTrackingRefAfterUpstreamUpdate(ctx context.Context, log *logpkg.Context, repo *serverRepo, ref refdb.Ref, params RefUpdateDownstreamParams, refConfig RefConfiguration) error {
	log = log.With("update-local-tracking-ref", params.RefIdentifier.Ref)
	level.Info(log).Log("params", params)

	timer := time.AfterFunc(1*time.Second, func() {
		level.Warn(log).Log("delay", "taking a long time, possible deadlock")
	})
	defer timer.Stop()

	// If this ref is configured to overwrite its upstream, then
	// refuse anything from the upstream except ops.
	//
	// TODO(sqs): in the future, provide a way like `git pull -f` for
	// users to explicitly accept overwrites from upstream.
	if refConfig.Overwrite && (params.Delete || params.State != nil) {
		level.Debug(log).Log("refusing-non-op-update", "")
		return nil
	}

	refClosure := repo.refdb.TransitiveClosureRefs(params.RefIdentifier.Ref)

	if params.Delete {
		if err := repo.refdb.Delete(params.RefIdentifier.Ref, ref, refdb.RefLogEntry{}); err != nil {
			return err
		}
	} else {
		if params.Current != nil {
			if err := compareRefBaseInfo(*params.Current, ref.Object.(serverRef)); err != nil {
				return &jsonrpc2.Error{
					Code:    int64(ErrorCodeRefConflict),
					Message: fmt.Sprintf("received upstream op for local tracking branch %q with conflicting ref state: %s", params.RefIdentifier.Ref, err),
				}
			}
		}

		oldRef := ref
		switch {
		case params.Ack:
			// State updates get acked, too, but those do not involve OT.
			if params.Op != nil {
				if err := ref.Object.(serverRef).ot.AckFromUpstream(); err != nil {
					if err == ws.ErrNoPendingOperation {
						level.Error(log).Log("received-ack-for-previous-generation-of-ref", "")
						// NOTE: ErrNoPendingOperation occurs when
						// this server's ref was recently updated but
						// its RefBaseInfo remains the same, and it
						// receives a slightly delayed upstream
						// update. It currently has no way to know
						// that the ack was for the previous ref.
						//
						// TODO(sqs): add a way to know we can
						// definitely ignore these, and make it so the
						// same problem could never occur when
						// receiving actual ops.
						return nil
					}
					return err
				}
			}

		case params.State != nil:
			// If this is the HEAD ref of a workspace, we need to go
			// via the workspace to reset the state, since we need to
			// change actual files on disk.
			isWorkspaceHEAD := false
			if headRef := repo.refdb.Lookup("HEAD"); headRef != nil && headRef.Target == ref.Name {
				isWorkspaceHEAD = true
				level.Info(log).Log("workspace-checkout", "")
				repo.mu.Lock()
				ws := repo.workspace
				repo.mu.Unlock()
				if ws == nil {
					panic(fmt.Sprintf("during local tracking ref update of %q, HEAD points to it but it has no workspace", ref.Name))
				}
				if err := ws.Checkout(ctx, log, false, ref.Name, params.State.GitBase, params.State.GitBranch, params.State.History, nil); err != nil {
					return fmt.Errorf("during local tracking ref update, workspace checkout failed: %s", err)
				}
			}

			oldRefObj := ref.Object.(serverRef)
			otHandler := &ws.Proxy{
				SendToUpstream: oldRefObj.ot.SendToUpstream,
				// TODO(sqs)!!! upstreamrevnumber should be 0 not len(history) beacuse we call RecvFromUpstream below, which will increment the upstreamrevnumber
				UpstreamRevNumber: len(params.State.History),
			}
			if !isWorkspaceHEAD {
				// Don't call Apply in our loop over
				// params.State.History, or else we'll double-apply
				// ops we just applied in the workspace Checkout call
				// above.
				otHandler.Apply = oldRefObj.ot.Apply
			}
			for _, op := range params.State.History {
				// OK to discard the RecvFromUpstream transformed op
				// return value because we know otHandler's history
				// started out empty (because we just created it).
				if _, err := otHandler.RecvFromUpstream(log, op); err != nil {
					return err
				}
			}
			if isWorkspaceHEAD {
				otHandler.Apply = oldRefObj.ot.Apply
			}
			ref.Object = serverRef{
				gitBase:   params.State.GitBase,
				gitBranch: params.State.GitBranch,
				ot:        otHandler,
			}

		case params.Op != nil:
			xop, err := ref.Object.(serverRef).ot.RecvFromUpstream(log, *params.Op)
			if err != nil {
				return err
			}
			params.Op = &xop
		}
		if err := repo.refdb.Write(ref, true, &oldRef, refdb.RefLogEntry{}); err != nil {
			return err
		}
	}

	// Don't broadcast acks to clients, since we already immediately
	// ack clients.
	if !params.Ack {
		if err := s.broadcastRefUpdateDownstream(ctx, log, params.RefIdentifier.Repo, withoutSymbolicRefs(refClosure), nil, params); err != nil {
			return err
		}
	}
	return nil
}

func compareRefPointerInfo(p RefPointer, r serverRef) error {
	var diffs []string
	if p.GitBase != r.gitBase {
		diffs = append(diffs, fmt.Sprintf("git base: %q != %q", p.GitBase, r.gitBase))
	}
	if p.GitBranch != r.gitBranch {
		diffs = append(diffs, fmt.Sprintf("git branch: %q != %q", p.GitBranch, r.gitBranch))
	}
	if p.Rev != r.ot.Rev() {
		diffs = append(diffs, fmt.Sprintf("rev: %d != %d", p.Rev, r.ot.Rev()))
	}
	if len(diffs) == 0 {
		return nil
	}
	return errors.New(strings.Join(diffs, ", "))
}

func compareRefBaseInfo(p RefBaseInfo, r serverRef) error {
	var diffs []string
	if p.GitBase != r.gitBase {
		diffs = append(diffs, fmt.Sprintf("git base: %q != %q", r.gitBase, p.GitBase))
	}
	if p.GitBranch != r.gitBranch {
		diffs = append(diffs, fmt.Sprintf("git branch: %q != %q", r.gitBranch, p.GitBranch))
	}
	if len(diffs) == 0 {
		return nil
	}
	return errors.New(strings.Join(diffs, ", "))
}

func (s *Server) startWorker(ctx context.Context) {
	log := s.baseLogger().With("worker", "")
	for {
		select {
		case f, ok := <-s.work:
			if !ok {
				return
			}
			if err := f(); err != nil {
				level.Error(log).Log("err", err)
			}

		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) broadcastRefUpdateDownstream(ctx context.Context, log *logpkg.Context, repo string, updatedRefs []refdb.Ref, sender *serverConn, params RefUpdateDownstreamParams) error {
	if ctx == nil {
		panic("ctx == nil")
	}

	const sendTimeout = 5 * time.Second

	sort.Sort(sortableRefs(updatedRefs))
	for _, ref_ := range updatedRefs {
		ref := ref_
		if ref.IsSymbolic() {
			panic(fmt.Sprintf("broadcastRefUpdateDownstream unexpectedly got symbolic ref %q", ref.Name))
		}
		refID := RefIdentifier{Repo: repo, Ref: ref.Name}
		if watchers := s.watchers(refID); len(watchers) > 0 {
			level.Debug(log).Log("broadcast-to-downstream-ref", refID, "watchers", strings.Join(clientIDs(watchers), " "), "downstream-params", params)

			// Determine if the sender is watching (so we don't send
			// both a broadcast AND an ack to the sender).
			var senderIsWatching bool
			for _, c := range watchers {
				if c == sender {
					senderIsWatching = true
					break
				}
			}

			s.work <- func() error {
				// Send the update with the ref name that the client
				// is watching as (e.g., "HEAD" not "master" if they
				// are only watching HEAD).
				params := params
				params.RefIdentifier.Ref = ref.Name

				par := parallel.NewRun(10)
				for _, c := range watchers {
					par.Acquire()
					go func(c *serverConn) {
						defer par.Release()

						if c == sender {
							// We ack the sender below (and must do that
							// after broadcasting updates to all
							// clients), so skip the sender here.
							return
						}

						ctx, cancel := context.WithTimeout(ctx, sendTimeout)
						defer cancel()
						if err := c.conn.Call(ctx, "ref/update", params, nil); err == io.ErrUnexpectedEOF {
							// This means the client connection no longer
							// exists. Continue sending to the other watchers.
						} else if err != nil {
							level.Warn(log).Log("watcher-broadcast-error", err, "id", c.init.ID)
							if err := c.conn.Close(); err != nil {
								level.Warn(log).Log("error-closing-watcher", err, "id", c.init.ID)
							}
							return
						}
					}(c)
				}
				if err := par.Wait(); err != nil {
					return err
				}

				if senderIsWatching && sender != nil {
					// Ack after the op has been sent to all clients.
					ackParams := params
					ackParams.Ack = true
					ctx, cancel := context.WithTimeout(ctx, sendTimeout)
					err := sender.conn.Call(ctx, "ref/update", ackParams, nil)
					cancel()
					if err == io.ErrUnexpectedEOF {
						// This means the sender disconnected.
						err = nil
					}
					if err != nil {
						return fmt.Errorf("acking to sender %q: %s", sender.init.ID, err)
					}
				}
				return nil
			}
		}
	}
	return nil
}

// broadcastRefUpdateSymbolic broadcasts an update to a symbolic ref
// to all of its watchers.
func (s *Server) broadcastRefUpdateSymbolic(ctx context.Context, log *logpkg.Context, repo *serverRepo, sender *serverConn, params RefUpdateSymbolicParams) error {
	const sendTimeout = 5 * time.Second

	refClosure := repo.refdb.TransitiveClosureRefs(params.RefIdentifier.Ref)
	sort.Sort(sortableRefs(refClosure))
	for _, ref_ := range refClosure {
		ref := ref_
		refID := RefIdentifier{Repo: params.RefIdentifier.Repo, Ref: ref.Name}
		if watchers := s.watchers(refID); len(watchers) > 0 {
			level.Debug(log).Log("broadcast-symbolic-ref-update", refID, "watchers", strings.Join(clientIDs(watchers), " "), "params", params)
			s.work <- func() error {
				// Send the update with the ref name that the client
				// is watching as (e.g., "HEAD" not "master" if they
				// are only watching HEAD).
				params := params
				params.RefIdentifier.Ref = ref.Name

				senderIsWatching := false
				for _, c := range watchers { // TODO(sqs): parallelize
					if c == sender {
						// We ack the sender below, so skip it here.
						senderIsWatching = true
						continue
					}

					// TODO(sqs): handle closed conns
					ctx, cancel := context.WithTimeout(ctx, sendTimeout)
					if err := c.conn.Call(ctx, "ref/updateSymbolic", params, nil); err == io.ErrUnexpectedEOF {
						// This means the client connection no longer
						// exists. Continue sending to the other watchers.
					} else if err != nil {
						cancel()
						level.Warn(log).Log("watcher-broadcast-error", err, "id", c.init.ID)
						if err := c.conn.Close(); err != nil {
							level.Warn(log).Log("error-closing-watcher", err, "id", c.init.ID)
						}
						continue
					}
					cancel()
				}

				if senderIsWatching && sender != nil {
					// Ack after the op has been sent to all clients.
					ackParams := params
					ackParams.Ack = true
					ctx, cancel := context.WithTimeout(ctx, sendTimeout)
					err := sender.conn.Call(ctx, "ref/updateSymbolic", ackParams, nil)
					cancel()
					if err == io.ErrUnexpectedEOF {
						// This means the sender disconnected.
						err = nil
					}
					if err != nil {
						return fmt.Errorf("acking to sender %q: %s", sender.init.ID, err)
					}
				}
				return nil
			}
		}
	}
	return nil
}

func (s *Server) handleSymbolicRefUpdate(ctx context.Context, log *logpkg.Context, sender *serverConn, repo *serverRepo, params RefUpdateSymbolicParams) error {
	// s.recvMu.Lock()
	// defer s.recvMu.Unlock()

	log = log.With("update-symbolic-ref", params.RefIdentifier.Ref, "old", params.OldTarget, "new", params.Target)
	level.Info(log).Log()

	timer := time.AfterFunc(1*time.Second, func() {
		level.Warn(log).Log("delay", "taking a long time, possible deadlock")
	})
	defer timer.Stop()

	newTargetRef := repo.refdb.Lookup(params.Target)
	if newTargetRef == nil {
		return &jsonrpc2.Error{
			Code:    int64(ErrorCodeRefNotExists),
			Message: fmt.Sprintf("update of symbolic ref %q to nonexistent ref %q", params.RefIdentifier.Ref, params.Target),
		}
	}
	if newTargetRef.IsSymbolic() {
		return &jsonrpc2.Error{
			Code:    int64(ErrorCodeSymbolicRefInvalid),
			Message: fmt.Sprintf("invalid update of symbolic ref %q target to symbolic ref %q (must be non-symbolic ref)", params.RefIdentifier.Ref, params.Target),
		}
	}

	var old *refdb.Ref
	if params.OldTarget != "" {
		old = &refdb.Ref{Name: params.RefIdentifier.Ref, Target: params.OldTarget}
	}
	if err := repo.refdb.Write(refdb.Ref{Name: params.RefIdentifier.Ref, Target: params.Target}, true, old, refdb.RefLogEntry{}); err != nil {
		if _, ok := err.(*refdb.WrongOldRefValueError); ok {
			return &jsonrpc2.Error{
				Code:    int64(ErrorCodeRefUpdateInvalid),
				Message: err.Error(),
			}
		}
		return err
	}

	return s.broadcastRefUpdateSymbolic(ctx, log, repo, sender, params)
}

func (s *Server) handleRefUpdateFromDownstream(ctx context.Context, log *logpkg.Context, repo *serverRepo, params RefUpdateUpstreamParams, sender *serverConn, applyLocally bool) error {
	// s.recvMu.Lock()
	// defer s.recvMu.Unlock()

	if err := params.validate(); err != nil {
		return &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInvalidParams,
			Message: "invalid params for ref update from downstream: " + err.Error(),
		}
	}

	if strings.HasPrefix(params.RefIdentifier.Ref, "refs/remotes/") {
		return &jsonrpc2.Error{
			Code:    int64(ErrorCodeRefUpdateInvalid),
			Message: fmt.Sprintf("remote tracking ref %q cannot be updated by a downstream (only by the upstream remote it tracks)", params.RefIdentifier.Ref),
		}
	}

	// TODO(sqs): HACK(sqs): fix same issue as with the other
	// s.updateFromDownstreamMu lock (see its call site comment for
	// more info). This will make it slower before it gets faster.
	s.updateFromDownstreamMu.Lock()
	defer s.updateFromDownstreamMu.Unlock()

	if sender != nil {
		log = log.With("update-ref-from-downstream", params.RefIdentifier.Ref)
	} else {
		log = log.With("update-ref-locally", params.RefIdentifier.Ref)
	}
	log = log.With("params", params)
	level.Info(log).Log("apply-locally", applyLocally)

	timer := time.AfterFunc(1*time.Second, func() {
		level.Warn(log).Log("delay", "taking a long time, possible deadlock")
	})
	defer timer.Stop()

	if ref := repo.refdb.Lookup(params.RefIdentifier.Ref); ref != nil && ref.IsSymbolic() && !params.Force {
		return &jsonrpc2.Error{
			Code:    int64(ErrorCodeRefUpdateInvalid),
			Message: fmt.Sprintf("a force-update is required to overwrite symbolic ref %q with a non-symbolic ref", params.RefIdentifier.Ref),
		}
	}

	ref, err := repo.refdb.Resolve(params.RefIdentifier.Ref)
	if err != nil {
		if e, ok := err.(*refdb.RefNotExistsError); ok && e.Name == params.RefIdentifier.Ref {
			// This is OK; it means we are creating the ref.
		} else {
			return err
		}
	}

	refClosure := repo.refdb.TransitiveClosureRefs(params.RefIdentifier.Ref)

	if params.Delete {
		// Delete ref.
		if ref == nil {
			return &jsonrpc2.Error{
				Code:    int64(ErrorCodeRefNotExists),
				Message: fmt.Sprintf("downstream sent ref deletion for nonexistent ref %q", params.RefIdentifier.Ref),
			}
		}
		if err := compareRefPointerInfo(*params.Current, ref.Object.(serverRef)); err != nil {
			return &jsonrpc2.Error{
				Code:    int64(ErrorCodeRefConflict),
				Message: fmt.Sprintf("downstream sent ref deletion with invalid current info: %s", err),
			}
		}
		if err := repo.refdb.Delete(params.RefIdentifier.Ref, *ref, refdb.RefLogEntry{}); err != nil {
			return err
		}
	} else {
		// Create or update ref.
		oldRef := ref
		if params.Current == nil {
			if ref != nil && !params.Force {
				return &jsonrpc2.Error{
					Code:    int64(ErrorCodeRefExists),
					Message: fmt.Sprintf("downstream sent ref update for existing ref %q, but neither current nor force was set on the update", params.RefIdentifier.Ref),
				}
			}
			ref = &refdb.Ref{Name: params.RefIdentifier.Ref, Object: serverRef{}}
		}
		if params.Current != nil {
			if ref == nil {
				return &jsonrpc2.Error{
					Code:    int64(ErrorCodeRefNotExists),
					Message: fmt.Sprintf("downstream sent ref update for nonexistent ref %q", params.RefIdentifier.Ref),
				}
			}
		}

		refObj := ref.Object.(serverRef)

		if params.Current != nil {
			if err := compareRefBaseInfo(params.Current.RefBaseInfo, ref.Object.(serverRef)); err != nil {
				return &jsonrpc2.Error{
					Code:    int64(ErrorCodeRefConflict),
					Message: fmt.Sprintf("downstream sent ref update with invalid current info: %s", err),
				}
			}

			tmp := *ref
			ref = &tmp
		}

		switch {
		case params.State != nil:
			// Propagate a non-op-only change upstream; otherwise we
			// will just append to the upstream's ref op history and
			// not actually reset it.
			if refConfig, ok := repo.config.Refs[params.RefIdentifier.Ref]; ok {
				if !refConfig.Overwrite {
					return &jsonrpc2.Error{
						Code:    int64(ErrorCodeRefUpdateInvalid),
						Message: fmt.Sprintf("refusing to perform a force-update/reset on ref %q that is not configured to overwrite its upstream", params.RefIdentifier.Ref),
					}
				}
				remote, ok := repo.config.Remotes[refConfig.Upstream]
				if !ok {
					return &jsonrpc2.Error{
						Code:    int64(ErrorCodeRemoteNotExists),
						Message: fmt.Sprintf("upstream remote %q configured for ref %s does not exist", refConfig.Upstream, params.RefIdentifier),
					}
				}
				cl, err := s.remotes.getOrCreateClient(ctx, log, remote.Endpoint)
				if err != nil {
					return err
				}
				if err := cl.RefUpdate(ctx, RefUpdateUpstreamParams{
					RefIdentifier: RefIdentifier{
						Repo: remote.Repo,
						Ref:  params.RefIdentifier.Ref,
					},
					Current: params.Current,
					Force:   params.Force, // TODO(sqs): should it always force?
					State:   params.State,
				}); err != nil {
					return err
				}
			}

			otHandler, err := s.backend.Create(ctx, log, params.RefIdentifier.Repo, params.State.GitBase)
			if err != nil {
				return err
			}
			if prevOT := refObj.ot; prevOT != nil {
				if otHandler.Apply == nil && prevOT.Apply != nil {
					// TODO(sqs): this is hacky, mainly for when our
					// mock tests have set an Apply and we want to
					// reuse it
					otHandler.Apply = prevOT.Apply
					level.Warn(log).Log("HACK-used-prev-ot-handler-Apply-func", "")
				}

				if otHandler.SendToUpstream != nil {
					// This should never happen, but just be safe.
					panic(fmt.Sprintf("new otHandler from backend %T already has SendToUpstream func", s.backend))
				}
				otHandler.SendToUpstream = prevOT.SendToUpstream
			}
			for _, op := range params.State.History {
				if applyLocally && otHandler.Apply != nil {
					if err := otHandler.Apply(log, op); err != nil {
						return err
					}
				}
				if err := otHandler.Record(op); err != nil {
					return err
				}
			}

			otHandler.UpstreamRevNumber = len(params.State.History)
			ref.Object = serverRef{
				gitBase:   params.State.GitBase,
				gitBranch: params.State.GitBranch,
				ot:        otHandler,
			}

			if oldRef == nil {
				// If this ref never existed before, then it wouldn't yet
				// exist in its own refClosure, so we must add it.
				refClosure = append(refClosure, *ref)
			}

		case params.Op != nil:
			if xop, err := refObj.ot.RecvFromDownstream(log, params.Current.Rev, *params.Op); err == nil {
				params.Op = &xop
			} else {
				return &jsonrpc2.Error{
					Code:    int64(ErrorCodeInvalidOp),
					Message: err.Error(),
				}
			}
		}

		if err := repo.refdb.Write(*ref, true, oldRef, refdb.RefLogEntry{}); err != nil {
			return err
		}

		// If we previously configured this ref to have an
		// upstream BEFORE this ref existed, then we need to check
		// now if we need to link the upstream to it.
		hasUpstreamConfigured := refObj.ot != nil && refObj.ot.SendToUpstream != nil
		if params.Op != nil && !hasUpstreamConfigured && params.Current == nil && params.State != nil {
			if c, ok := repo.config.Refs[params.RefIdentifier.Ref]; ok {
				level.Info(log).Log("reattached-ref-config-to-newly-created-ref", fmt.Sprint(c))
				if err := s.doUpdateRefConfiguration(ctx, log, repo, params.RefIdentifier, ref, RefConfiguration{}, c); err != nil {
					return err
				}
			}
		}
	}

	toRefBaseInfo := func(p *RefPointer) *RefBaseInfo {
		if p == nil {
			return nil
		}
		return &RefBaseInfo{GitBase: p.GitBase, GitBranch: p.GitBranch}
	}
	return s.broadcastRefUpdateDownstream(ctx, log, params.RefIdentifier.Repo, withoutSymbolicRefs(refClosure), sender, RefUpdateDownstreamParams{
		RefIdentifier: params.RefIdentifier,
		Current:       toRefBaseInfo(params.Current),
		State:         params.State,
		Op:            params.Op,
		Delete:        params.Delete,
	})
}

func clientIDs(conns []*serverConn) (ids []string) {
	ids = make([]string, len(conns))
	for i, c := range conns {
		c.mu.Lock()
		if c.init != nil {
			ids[i] = c.init.ID
		}
		c.mu.Unlock()
	}
	sort.Strings(ids)
	return ids
}

type sortableRefs []refdb.Ref

func (v sortableRefs) Len() int           { return len(v) }
func (v sortableRefs) Swap(i, j int)      { v[i], v[j] = v[j], v[i] }
func (v sortableRefs) Less(i, j int) bool { return v[i].Name < v[j].Name }

func withoutSymbolicRefs(refs []refdb.Ref) []refdb.Ref {
	x := refs[:0]
	for _, ref := range refs {
		if !ref.IsSymbolic() {
			x = append(x, ref)
		}
	}
	return x
}
