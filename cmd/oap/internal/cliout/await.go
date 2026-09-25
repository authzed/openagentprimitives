package cliout

import (
	"context"
	"fmt"
	"io"
	"time"
)

// spinnerFrames animate a single-cell braille spinner.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

const (
	awaitFirstBackoff = 2 * time.Second
	awaitMaxBackoff   = 20 * time.Second
	awaitFrameRate    = 100 * time.Millisecond
)

// Await polls fn until it reports done (true), fn returns a non-nil error, or
// the deadline elapses. Between polls it backs off exponentially (2s, doubling,
// capped at 20s) so a multi-minute wait — e.g. a cloud load balancer that takes
// minutes to get an address — does not hammer the API. On a terminal it renders
// an animated spinner on a single rewriting line that shows the elapsed time and
// a live countdown to the next poll, so the wait reads as progress rather than a
// hang; on a non-terminal (piped/CI) it prints one plain status line per poll
// instead. Returns fn's error, or ctx.Err() (DeadlineExceeded / cancellation)
// when the deadline elapses first. The spinner line is cleared before returning
// so the caller's next output starts on a clean line.
func Await(ctx context.Context, w io.Writer, message string, deadline time.Duration, fn func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	tty := colorize(w)
	start := time.Now()
	backoff := awaitFirstBackoff

	for {
		done, err := fn(ctx)
		if tty {
			fmt.Fprint(w, "\r\033[K") // clear the spinner line before narrating/returning
		}
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		nextAt := time.Now().Add(backoff)
		if tty {
			if err := spin(ctx, w, message, start, nextAt); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(w, "%s (%s elapsed, next check in %s)\n",
				message, time.Since(start).Round(time.Second), backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		if backoff < awaitMaxBackoff {
			if backoff *= 2; backoff > awaitMaxBackoff {
				backoff = awaitMaxBackoff
			}
		}
	}
}

// spin animates the spinner line on a terminal until nextAt is reached or ctx is
// cancelled (returning ctx.Err()), clearing the line on cancellation.
func spin(ctx context.Context, w io.Writer, message string, start, nextAt time.Time) error {
	ticker := time.NewTicker(awaitFrameRate)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			fmt.Fprint(w, "\r\033[K")
			return ctx.Err()
		case <-ticker.C:
			if !time.Now().Before(nextAt) {
				return nil
			}
			fmt.Fprintf(w, "\r%s%c%s %s (%s elapsed, next check in %s)",
				cyan, spinnerFrames[i%len(spinnerFrames)], reset, message,
				time.Since(start).Round(time.Second), time.Until(nextAt).Round(time.Second))
		}
	}
}
