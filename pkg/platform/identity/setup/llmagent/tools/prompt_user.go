package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// PromptUserPrompter is the seam for asking the user. Tests inject a scripted
// answerer.
type PromptUserPrompter func(ctx context.Context, message string, kind string, choices []string, defaultVal string,
	stdin io.Reader, stdout io.Writer) (string, error)

var PromptUser PromptUserPrompter = defaultPromptUser

const PromptUserName = "prompt_user"

const PromptUserSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "message":  { "type": "string" },
    "kind":     { "type": "string", "enum": ["text", "secret", "choice"] },
    "choices":  { "type": "array", "items": { "type": "string" } },
    "default":  { "type": "string" }
  },
  "required": ["message", "kind"]
}`

// PromptUserRun delegates to the package-level prompter.
func PromptUserRun(ctx context.Context, raw json.RawMessage, stdin io.Reader, stdout io.Writer) (string, error) {
	var args struct {
		// The question put to the user; required.
		Message string `json:"message"`
		// One of text, secret, or choice; anything else is refused.
		Kind string `json:"kind"`
		// The options offered for kind=choice; required there, ignored otherwise.
		Choices []string `json:"choices"`
		// Pre-selected answer, used when the user just hits enter; empty means none.
		Default string `json:"default"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("prompt_user: parse: %w", err)
	}
	if args.Message == "" {
		return "", fmt.Errorf("prompt_user: message required")
	}
	switch args.Kind {
	case "text", "secret", "choice":
	default:
		return "", fmt.Errorf("prompt_user: kind must be text/secret/choice")
	}
	if args.Kind == "choice" && len(args.Choices) == 0 {
		return "", fmt.Errorf("prompt_user: kind=choice requires choices[]")
	}
	return PromptUser(ctx, args.Message, args.Kind, args.Choices, args.Default, stdin, stdout)
}

// defaultPromptUser is the production prompter: plain readline for text and
// secret modes (secret may not hide when stdin is not a TTY — intentional, for
// testability) and a numbered menu for choice mode.
func defaultPromptUser(ctx context.Context, message, kind string, choices []string, defaultVal string,
	stdin io.Reader, stdout io.Writer) (string, error) {
	switch kind {
	case "secret":
		return readSecretLine(message, stdin, stdout)
	case "choice":
		return readChoice(message, choices, defaultVal, stdin, stdout)
	default:
		return readPlainLine(message, defaultVal, stdin, stdout)
	}
}

func readPlainLine(message, defaultVal string, stdin io.Reader, stdout io.Writer) (string, error) {
	if defaultVal != "" {
		fmt.Fprintf(stdout, "%s [%s]: ", message, defaultVal)
	} else {
		fmt.Fprintf(stdout, "%s: ", message)
	}
	var s string
	if _, err := fmt.Fscanln(stdin, &s); err != nil && err.Error() != "unexpected newline" {
		return "", err
	}
	if s == "" {
		s = defaultVal
	}
	return s, nil
}

// readSecretLine reads a single line without echoing when stdin is the process's
// TTY. A non-file Reader (as tests pass) falls back to fmt.Fscanln, which DOES
// echo — bounded to test output, since tests do not run on a real TTY.
func readSecretLine(message string, stdin io.Reader, stdout io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(stdout, "%s: ", message)
		raw, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			return "", err
		}
		fmt.Fprintln(stdout)
		return string(raw), nil
	}
	fmt.Fprintf(stdout, "%s: ", message)
	var s string
	if _, err := fmt.Fscanln(stdin, &s); err != nil && err.Error() != "unexpected newline" {
		return "", err
	}
	return s, nil
}

func readChoice(message string, choices []string, defaultVal string, stdin io.Reader, stdout io.Writer) (string, error) {
	fmt.Fprintf(stdout, "%s\n", message)
	for i, c := range choices {
		marker := " "
		if c == defaultVal {
			marker = "*"
		}
		fmt.Fprintf(stdout, " [%d]%s %s\n", i+1, marker, c)
	}
	fmt.Fprintf(stdout, "Enter a number (1-%d): ", len(choices))
	var n int
	if _, err := fmt.Fscanln(stdin, &n); err != nil {
		return "", fmt.Errorf("prompt_user: read choice: %w", err)
	}
	if n < 1 || n > len(choices) {
		return "", fmt.Errorf("prompt_user: choice %d out of range", n)
	}
	return choices[n-1], nil
}
