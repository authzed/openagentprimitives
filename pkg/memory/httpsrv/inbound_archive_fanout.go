package httpsrv

import (
	"context"
	"errors"
	"fmt"

	"path"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/extract"

	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// explodeInboundArchive fans an archive out into individually-stored members
// and replaces its "extracted text" with an index naming them.
//
// Returns false when this upload is not an archive we handle, so the caller
// takes the ordinary single-file extraction path unchanged. Returns true once
// it has taken ownership of the response — including the permanent-failure
// cases, where resp reports Unsupported and the archive's own bytes remain
// stored and addressable.
//
// The shape mirrors extractInboundAsset one level out: the archive's bytes are
// already stored, and this re-reads them with a fresh Get rather than holding a
// second copy of the request body. Each member is then stored and run through
// the SAME extractor call a directly-uploaded file gets, so a member costs
// exactly what that file would have cost and there is no parallel path to keep
// in sync.
func (h *handler) explodeInboundArchive(ctx context.Context, ns, sess, assetID, mime, filename string, ref artifactstore.Ref, resp *inboundAssetResponse) bool {
	if h.attachmentExploder == nil {
		return false
	}

	rc, err := h.artifactStore.Get(ctx, ref)
	if err != nil {
		logInboundAssetError(ctx, "re-read stored archive for explode failed", ns, sess, filename, err)
		// TRANSIENT: the bytes are stored, we simply could not read them back.
		// Reporting Unsupported here would tell the user a permanent falsehood.
		return false
	}
	defer func() { _ = rc.Close() }()

	var members []memberResult
	var idx int
	lim := h.archiveLimitsFor(ctx, ns, sess)
	sum, xerr := h.attachmentExploder.Explode(ctx, mime, rc, lim, func(m ExplodedMember) error {
		mr, merr := h.storeArchiveMember(ctx, ns, sess, assetID, idx, m)
		idx++
		if merr != nil {
			// One member failing to store must not fail the archive: 186 good
			// files are still worth having. The member is simply absent from
			// the index, and the failure is logged with session context.
			logInboundAssetError(ctx, "store archive member failed", ns, sess, filename, merr)
			return nil
		}
		members = append(members, mr)
		return nil
	})

	if xerr != nil {
		switch {
		case errors.Is(xerr, ErrArchiveMIMEUnsupported):
			// No exploder claims this container type. PERMANENT, and not the
			// same statement as "we refused to open it".
			return false
		case errors.Is(xerr, ErrArchiveRefused):
			// A bound tripped, or the bytes are not a readable archive.
			// PERMANENT: the archive's raw bytes stay stored and addressable,
			// but nothing was extracted from them.
			resp.Unsupported = true
			log.FromContext(ctx).Info("archive refused",
				"namespace", ns, "session", sess, "filename", filename, "err", xerr.Error())
			return true
		default:
			// TRANSIENT. Leaving resp in the Extracted=false, Unsupported=false
			// shape is what tells the caller to word it as a temporary failure.
			logInboundAssetError(ctx, "archive explode failed", ns, sess, filename, xerr)
			return true
		}
	}

	// The index IS the archive's extracted text, which is why the archive
	// takes the ordinary read path from here: the caller's manifest line and
	// the runner's hydration both key on TextRef and need no archive-specific
	// case.
	indexKey := path.Join(ns, sess, "inbound-asset", assetID, "index.txt")
	indexRef, ierr := h.artifactStore.Put(ctx, indexKey,
		strings.NewReader(renderArchiveIndex(filename, members, sum)))
	if ierr != nil {
		logInboundAssetError(ctx, "store archive index failed", ns, sess, filename, ierr)
		return true // transient shape: bytes stored, nothing extracted
	}

	resp.Extracted = true
	resp.TextRef = string(indexRef)
	resp.Members = members
	resp.ArchiveTruncated = sum.Truncated
	resp.ArchiveTruncatedReason = sum.TruncatedReason
	resp.ArchiveTruncatedMember = sum.TruncatedMember
	log.FromContext(ctx).Info("archive exploded",
		"namespace", ns, "session", sess, "filename", filename,
		"members", len(members), "truncated", sum.Truncated)
	return true
}

// storeArchiveMember stores one member's bytes and, when an extractor claims
// its MIME, its text — the same two-step a directly-uploaded file takes.
//
// The member's body is a LIVE reader valid only for this call, so it is
// streamed straight into Put and never buffered.
func (h *handler) storeArchiveMember(ctx context.Context, ns, sess, assetID string, idx int, m ExplodedMember) (memberResult, error) {
	// The member index, not its name, keys the object. A name is
	// attacker-controlled and two members can normalize to one string; an
	// index cannot collide, and the name still rides in the result for the
	// agent to read.
	key := path.Join(ns, sess, "inbound-asset", assetID, "members",
		fmt.Sprintf("%04d", idx), storageFilename(sanitizeUploadFilename(path.Base(m.Name))))

	ref, err := h.artifactStore.Put(ctx, key, m.Body)
	if err != nil {
		return memberResult{}, err
	}

	out := memberResult{
		Name:      m.Name,
		MIME:      m.MIME,
		SizeBytes: m.Size,
		Ref:       string(ref),
	}

	// Nesting depth is 0: a member that is itself a container is stored and
	// classified, never opened. extractord holds the exploder registry and
	// says so on the wire, so this side keeps no parallel list of container
	// MIMEs to drift from.
	explodable := m.IsArchive
	if !explodable && h.attachmentExtractor != nil {
		if text, pages, eerr := h.extractMemberText(ctx, ref, m.MIME); eerr == nil {
			textKey := path.Join(ns, sess, "inbound-asset", assetID, "members",
				fmt.Sprintf("%04d", idx), "text.txt")
			if textRef, terr := h.artifactStore.Put(ctx, textKey, strings.NewReader(text)); terr == nil {
				out.TextRef = string(textRef)
				out.Pages = pages
			} else {
				logInboundAssetError(ctx, "store archive member text failed", ns, sess, m.Name, terr)
			}
		} else if !errors.Is(eerr, ErrAttachmentMIMEUnsupported) {
			// Unsupported is the ordinary case for a log file; anything else
			// is worth a line so an operator can see extraction degrading.
			logInboundAssetError(ctx, "archive member extraction failed", ns, sess, m.Name, eerr)
		}
	}

	out.Readability = readabilityOf(out, explodable)
	return out, nil
}

// extractMemberText re-reads a stored member and extracts its text. A fresh
// Get, never a second copy of the member's bytes, mirroring the single-file
// path's own discipline.
func (h *handler) extractMemberText(ctx context.Context, ref artifactstore.Ref, mime string) (string, int, error) {
	rc, err := h.artifactStore.Get(ctx, ref)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rc.Close() }()
	return h.attachmentExtractor.Extract(ctx, mime, rc)
}

// archiveLimitsFor resolves the bounds this session's AgentClass asks for.
//
// Resolved HERE rather than passed in by channelsd, because this is where the
// value is used and the operator already holds a Kubernetes client. Threading
// it through the upload call would add a parameter to an interface the e2e
// harness implements, for a value the caller has no other use for.
//
// FAIL-OPEN TO THE CEILING, deliberately. Every failure path — no client, a
// missing session, a deleted class, a malformed config — returns
// extract.DefaultLimits, which is the STRICTEST this deployment serves. A class
// can only ever tighten from there, so failing to read it can never widen what
// gets opened; the worst case is that an operator's tightening is not applied,
// which is logged rather than silent.
func (h *handler) archiveLimitsFor(ctx context.Context, ns, sess string) extract.Limits {
	if h.k8sClient == nil {
		return extract.DefaultLimits
	}

	var session spiceboxv1alpha1.AgentSession
	if err := h.k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: sess}, &session); err != nil {
		log.FromContext(ctx).Info("archive limits: session lookup failed; the deployment ceiling applies",
			"namespace", ns, "session", sess, "err", err.Error())
		return extract.DefaultLimits
	}
	if session.Spec.Class == "" {
		return extract.DefaultLimits
	}

	var class spiceboxv1alpha1.AgentClass
	if err := h.k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: session.Spec.Class}, &class); err != nil {
		log.FromContext(ctx).Info("archive limits: class lookup failed; the deployment ceiling applies",
			"namespace", ns, "class", session.Spec.Class, "err", err.Error())
		return extract.DefaultLimits
	}

	raw, ok := class.Spec.Capabilities[attachmentsCapabilityName]
	if !ok {
		return extract.DefaultLimits
	}
	lim, err := extract.ParseLimits(raw.Raw, extract.DefaultLimits)
	if err != nil {
		// Already rejected at apply time by the capability's ParseConfig, so
		// reaching here means the class was applied by something that skipped
		// that path. Never silent.
		log.FromContext(ctx).Info("archive limits: class config is unusable; the deployment ceiling applies",
			"namespace", ns, "class", session.Spec.Class, "err", err.Error())
		return extract.DefaultLimits
	}
	return lim
}

// attachmentsCapabilityName is the AgentClass capability key. A local constant
// rather than an import of pkg/agent/tool/meta/capability, which pulls in the
// runner's whole tool graph — the same reason channelsd's pipeline keeps its
// own copy.
const attachmentsCapabilityName = "attachments"
