package goals

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ReportPolicy is explicit consent to publish agent-reported data privately.
// It does not certify the data as an independently verified external fact.
type ReportPolicy struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

type ObservationReport struct {
	Kind    string          `json:"kind"`
	Subject string          `json:"subject"`
	Data    json.RawMessage `json:"data"`
}

func (p ReportPolicy) Validate() error {
	for _, v := range []string{p.Kind, p.Subject} {
		if strings.TrimSpace(v) == "" || len(v) > 1024 {
			return fmt.Errorf("%w: report kind and subject are required", ErrInvalid)
		}
	}
	return nil
}

func (r ObservationReport) Validate() error {
	if err := (ReportPolicy{Kind: r.Kind, Subject: r.Subject}).Validate(); err != nil {
		return err
	}
	if len(r.Data) == 0 || len(r.Data) > 32768 || !json.Valid(r.Data) {
		return fmt.Errorf("%w: observation requires valid JSON within 32768 bytes", ErrInvalid)
	}
	return nil
}

// ReportOutbox survives runner cleanup and worker turnover. Publication and
// acknowledgement are idempotent; transient failures remain retryable.
// Permanent loss of authority discards publication with an audit record.
type ReportOutbox interface {
	PendingReportedObservations(context.Context, int) ([]Occurrence, error)
	AcknowledgeReportedObservation(context.Context, string) error
	DiscardReportedObservation(context.Context, string, string, time.Time) error
}
