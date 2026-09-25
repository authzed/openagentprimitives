package identitycmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

// verifyGate applies the put-token CLI policy to a live-verification
// outcome before a credential is stored:
//
//   - Valid          → proceed (print who the token authenticated as)
//   - Unsupported    → proceed quietly (no verifier available; not a warning)
//   - Indeterminate  → warn + proceed (never block an unknown)
//   - Rejected + TTY → warn + "Store it anyway? [y/N]"
//   - Rejected, no TTY → fail closed; --skip-verify overrides
//   - Forbidden      → same policy as Rejected, different words: the provider
//     took the credential and refused this one check, so an operator is asked
//     rather than told their credential is dead
//
// skipVerify skips ONLY the live check — the ValidateToken format gate
// runs before this in every caller and has no override.
//
// It returns the verification result alongside its verdict so a caller that is
// about to STORE the credential can record what the check observed — which
// provider account the token authenticated as. The zero result on the
// --skip-verify path is the honest answer: nothing was checked, so nothing is
// attested.
func verifyGate(ctx context.Context, out io.Writer, prov *provider.Provider,
	value builtins.StoreValue, skipVerify bool, stdin io.Reader, interactive bool) (builtins.VerifyResult, error) {

	if skipVerify {
		fmt.Fprintln(out, "warning: skipping live token verification (--skip-verify)")
		return builtins.VerifyResult{}, nil
	}
	res := builtins.VerifyCredential(ctx, prov, value)
	switch res.Status {
	case builtins.VerifyValid:
		if res.Detail != "" {
			fmt.Fprintf(out, "token verified: %s\n", res.Detail)
		}
		return res, nil
	case builtins.VerifyIndeterminate:
		fmt.Fprintf(out, "warning: could not verify the token (%s); storing anyway\n", res.Detail)
		return res, nil
	case builtins.VerifyUnsupported:
		fmt.Fprintf(out, "note: no live verification available for this provider; storing\n")
		return res, nil
	}
	// Everything else — Rejected today, plus any VerifyStatus added later —
	// lands here. There is deliberately NO default: arm above: the three cases
	// that return are the only ones allowed to store unattended, so falling out
	// of the switch IS the conservative path and a new verdict fails closed
	// rather than storing in silence.
	//
	// The wording is still per-verdict, because the policy being the same does
	// not make the verdicts the same: only Rejected is the provider saying the
	// credential is bad.
	var summary string
	switch res.Status {
	case builtins.VerifyRejected:
		summary = "the provider rejected this token: " + res.Detail
		fmt.Fprintf(out, "token verification failed: %s\n", res.Detail)
	case builtins.VerifyForbidden:
		// NOT a rejection: the provider took the credential and refused this
		// one check. It still stops an unattended store — an operator running
		// put-token wants to know before the credential is provisioned — but
		// the wording must not tell them a working credential is dead.
		summary = builtins.ForbiddenNotice(res)
		fmt.Fprintf(out, "warning: %s\n", summary)
	default:
		summary = builtins.UnrecognizedNotice(res)
		fmt.Fprintf(out, "warning: %s\n", summary)
	}
	if interactive {
		ok, err := confirmYN(stdin, out, "Store it anyway?")
		if err != nil {
			return res, fmt.Errorf("token unconfirmed and confirmation unavailable: %w (pass --skip-verify to store anyway)", err)
		}
		if !ok {
			return res, fmt.Errorf("%s; not stored", summary)
		}
		// Stored on a human's say-so, not on the provider's. res carries no
		// subject id on these verdicts, so nothing is attested — which is
		// correct: the account this token belongs to was never established.
		return res, nil
	}
	return res, fmt.Errorf("refusing to store an unconfirmed token: %s (pass --skip-verify to store anyway)", summary)
}

// confirmYN prints "<prompt> [y/N] " to w and reads one line from r,
// byte-at-a-time so a call never over-reads into bytes belonging to a later
// call sharing the same reader (a bufio.Reader would buffer past the newline
// and discard those bytes when dropped).
//
// Returns true only for y/yes (case-insensitive); anything else — including an
// empty line — is false (default no). EOF before any line is an ERROR, which is
// what makes it usable as a gate: verifyGate must be able to tell "the operator
// declined" from "nobody was there to ask", and only the second may not store.
//
// Deliberately not a tui.Question run through a Driver, for two reasons. It is
// not part of a screen sequence — it is one yes/no reached partway through a
// command that has already written prose to this stream, so a run with its own
// rail and summary would frame a two-line policy check as a wizard. And the
// line-oriented driver hands the field to huh's accessible renderer, which
// turns end-of-input into the field's DEFAULT with a nil error: the unattended
// case would come back "declined" or "accepted" rather than as the error this
// gate is built on.
func confirmYN(r io.Reader, w io.Writer, prompt string) (bool, error) {
	fmt.Fprintf(w, "%s [y/N] ", prompt)
	var b [1]byte
	var line []byte
	read := false
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			read = true
			if b[0] == '\n' {
				break
			}
			line = append(line, b[0])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return false, fmt.Errorf("read confirmation: %w", err)
		}
	}
	if !read {
		return false, fmt.Errorf("read confirmation: %w", io.EOF)
	}
	switch strings.ToLower(strings.TrimSpace(string(line))) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
