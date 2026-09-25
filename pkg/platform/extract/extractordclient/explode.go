package extractordclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

var _ httpsrv.AttachmentExploder = (*Client)(nil)

// explodeTimeout backstops an archive round trip independently of the caller's
// context, the same way defaultTimeout does for /extract.
//
// Longer than defaultTimeout because the work is genuinely larger: one request
// carries the whole archive up and every member back down, where /extract
// carries one file each way. It is deliberately shorter than channelsd's own
// archive budget, so the transport gives up before the caller does and the
// caller's error says which side failed.
const explodeTimeout = 4 * time.Minute

// Wire header names, matching internal/cmd/extractord's explode.go.
const (
	hdrMemberName     = "X-Member-Name"
	hdrMemberSize     = "X-Member-Size"
	hdrMemberKind     = "X-Member-Kind"
	hdrMemberArchive  = "X-Member-Archive"
	memberKindSummary = "summary"
)

// wireSummary mirrors extractord's explodeSummary.
type wireSummary struct {
	Members         int    `json:"members"`
	Truncated       bool   `json:"truncated,omitempty"`
	TruncatedReason string `json:"truncatedReason,omitempty"`
	// TruncatedMember names the member whose copy was cut. Carried through so
	// a consumer can mark THAT row partial rather than storing and indexing a
	// half-file as whole -- see extract.Summary.TruncatedMember.
	TruncatedMember string `json:"truncatedMember,omitempty"`
	Skipped         []struct {
		Reason string `json:"reason"`
		Count  int    `json:"count"`
	} `json:"skipped,omitempty"`
}

// Explode POSTs body to extractord's /explode route and calls yield once per
// member, streaming.
//
// Each member's Body is the LIVE multipart part, handed to yield and then
// advanced past. It is never buffered here: an archive may expand to tens of
// megabytes across hundreds of members, and holding them would put the whole
// expansion in the operator's heap.
//
// Maps extractord's status contract onto the permanent/transient split the
// notice wording depends on: 415 and 413/422 are permanent and distinguishable
// from each other; every other non-2xx and every transport failure is
// transient and must never be re-worded as "unsupported".
func (c *Client) Explode(ctx context.Context, mimeType string, body io.Reader, lim extract.Limits, yield func(httpsrv.ExplodedMember) error) (httpsrv.ExplodeSummary, error) {
	var out httpsrv.ExplodeSummary

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/explode", body)
	if err != nil {
		return out, fmt.Errorf("extractordclient: build explode request: %w", err)
	}
	req.Header.Set("Content-Type", mimeType)

	// The bounds ride as headers, and extractord RE-CLAMPS them to its own
	// ceiling. Sending them is a request to tighten, never a grant: the pod
	// that opens the archive is the only one that can enforce anything, so it
	// must not inherit a limit from whoever called it.
	setLimitHeaders(req.Header, lim)

	client := c.http
	if client.Timeout < explodeTimeout {
		// A copy, not a mutation: c.http is shared with Extract, whose own
		// budget is deliberately smaller.
		cp := *client
		cp.Timeout = explodeTimeout
		client = &cp
	}

	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("extractordclient: explode request failed: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnsupportedMediaType:
		return out, httpsrv.ErrArchiveMIMEUnsupported
	case http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return out, fmt.Errorf("%w (status %d)", httpsrv.ErrArchiveRefused, resp.StatusCode)
	default:
		// Transient. Deliberately NOT wrapped in either sentinel: a caller that
		// treated a 500 as "unsupported type" would tell the user a permanent
		// falsehood about a file that would work on the next attempt.
		return out, fmt.Errorf("extractordclient: explode returned status %d", resp.StatusCode)
	}

	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return out, fmt.Errorf("extractordclient: explode response is not multipart")
	}

	mr := multipart.NewReader(resp.Body, params["boundary"])
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return out, fmt.Errorf("extractordclient: reading explode response: %w", perr)
		}

		if part.Header.Get(hdrMemberKind) == memberKindSummary {
			var ws wireSummary
			if derr := json.NewDecoder(part).Decode(&ws); derr != nil {
				_ = part.Close()
				return out, fmt.Errorf("extractordclient: decoding explode summary: %w", derr)
			}
			_ = part.Close()
			out.Members = ws.Members
			out.Truncated = ws.Truncated
			out.TruncatedReason = ws.TruncatedReason
			out.TruncatedMember = ws.TruncatedMember
			if len(ws.Skipped) > 0 {
				out.Skipped = make(map[string]int, len(ws.Skipped))
				for _, s := range ws.Skipped {
					out.Skipped[s.Reason] = s.Count
				}
			}
			continue
		}

		size, _ := strconv.ParseInt(part.Header.Get(hdrMemberSize), 10, 64)
		yerr := yield(httpsrv.ExplodedMember{
			Name:      part.Header.Get(hdrMemberName),
			MIME:      part.Header.Get("Content-Type"),
			Size:      size,
			Body:      part,
			IsArchive: part.Header.Get(hdrMemberArchive) != "",
		})
		_ = part.Close()
		if yerr != nil {
			return out, yerr
		}
	}
	return out, nil
}

// Limit headers, matching internal/cmd/extractord's explode.go.
const (
	hdrLimitUncompressedTotal = "X-Limit-Uncompressed-Total"
	hdrLimitMemberBytes       = "X-Limit-Member-Bytes"
	hdrLimitMembers           = "X-Limit-Members"
	hdrLimitRatio             = "X-Limit-Ratio"
)

// setLimitHeaders writes the requested bounds. A non-positive value is OMITTED
// rather than sent: extractord reads a missing header as "use the default",
// and sending a zero would depend on the far side reading it the same way.
func setLimitHeaders(h http.Header, lim extract.Limits) {
	if lim.MaxUncompressedTotal > 0 {
		h.Set(hdrLimitUncompressedTotal, strconv.FormatInt(lim.MaxUncompressedTotal, 10))
	}
	if lim.MaxMemberBytes > 0 {
		h.Set(hdrLimitMemberBytes, strconv.FormatInt(lim.MaxMemberBytes, 10))
	}
	if lim.MaxMembers > 0 {
		h.Set(hdrLimitMembers, strconv.Itoa(lim.MaxMembers))
	}
	if lim.MaxRatio > 0 {
		h.Set(hdrLimitRatio, strconv.Itoa(lim.MaxRatio))
	}
}
