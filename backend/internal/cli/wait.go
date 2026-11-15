package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The wait DTOs mirror the daemon's CreateWaitRequest / WaitResponse shapes
// without importing the controller package.
type waitAPIRequest struct {
	Kind            string `json:"kind"`
	PRNumber        int    `json:"prNumber,omitempty"`
	PRURL           string `json:"prUrl,omitempty"`
	For             string `json:"for,omitempty"`
	DurationSeconds int64  `json:"durationSeconds,omitempty"`
	TimeoutSeconds  int64  `json:"timeoutSeconds,omitempty"`
	Note            string `json:"note,omitempty"`
}

type waitDTO struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"sessionId"`
	Kind       string     `json:"kind"`
	For        string     `json:"for,omitempty"`
	PRNumber   int        `json:"prNumber,omitempty"`
	PRURL      string     `json:"prUrl,omitempty"`
	Note       string     `json:"note,omitempty"`
	State      string     `json:"state"`
	Outcome    string     `json:"outcome,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	FireAt     *time.Time `json:"fireAt,omitempty"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

type createWaitAPIResponse struct {
	Wait     waitDTO `json:"wait"`
	Existing bool    `json:"existing,omitempty"`
}

type listWaitsAPIResponse struct {
	Waits []waitDTO `json:"waits"`
}

type waitEnvelopeDTO struct {
	Wait waitDTO `json:"wait"`
}

var waitConditions = []string{"checks", "merge", "review", "any"}

func newWaitCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Wait for a PR's CI, merge or review, or a timer, without polling",
		Long: "Register what this session is waiting for, then end your turn: AO messages the session once,\n" +
			"with the complete result, when it happens. Replaces sleep-and-poll loops on gh pr view / gh run view.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return usageError{errors.New("usage: ao wait pr <number> | ao wait time <duration> | ao wait ls | ao wait cancel <wait-id>")}
		},
	}
	cmd.AddCommand(newWaitPRCommand(ctx), newWaitTimeCommand(ctx), newWaitListCommand(ctx), newWaitCancelCommand(ctx))
	return cmd
}

func newWaitPRCommand(ctx *commandContext) *cobra.Command {
	var condition, note, timeout string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "pr <number|url>",
		Short: "Wake this session when a PR's checks finish, it merges or closes, or a review arrives",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := waitSessionID()
			if err != nil {
				return err
			}
			req := waitAPIRequest{Kind: "pr", Note: note}
			if req.PRNumber, req.PRURL, err = parseWaitPR(args[0]); err != nil {
				return err
			}
			req.For = strings.ToLower(strings.TrimSpace(condition))
			if !slices.Contains(waitConditions, req.For) {
				return usageError{fmt.Errorf("usage: --for must be one of %s", strings.Join(waitConditions, ", "))}
			}
			if timeout != "" {
				d, err := parseWaitDuration(timeout)
				if err != nil {
					return usageError{fmt.Errorf("usage: --timeout: %w", err)}
				}
				req.TimeoutSeconds = int64(d / time.Second)
			}
			var res createWaitAPIResponse
			if err := ctx.postJSON(cmd.Context(), waitsPath(session), req, &res); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writeWaitRegistered(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&condition, "for", "checks", "What to wait for: checks, merge, review, or any (a merge or close always wakes)")
	cmd.Flags().StringVar(&note, "note", "", "Why you are waiting; echoed back in the wake message")
	cmd.Flags().StringVar(&timeout, "timeout", "", "Give up after this long, e.g. 6h (default 24h)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print the registered wait as JSON")
	return cmd
}

func newWaitTimeCommand(ctx *commandContext) *cobra.Command {
	var note string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "time <duration>",
		Short: "Wake this session after a duration, e.g. 20m or 1h",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := waitSessionID()
			if err != nil {
				return err
			}
			d, err := parseWaitDuration(args[0])
			if err != nil {
				return usageError{fmt.Errorf("usage: ao wait time <duration>: %w", err)}
			}
			var res createWaitAPIResponse
			if err := ctx.postJSON(cmd.Context(), waitsPath(session), waitAPIRequest{Kind: "time", DurationSeconds: int64(d / time.Second), Note: note}, &res); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writeWaitRegistered(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "Why you are waiting; echoed back in the wake message")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print the registered wait as JSON")
	return cmd
}

func newWaitListCommand(ctx *commandContext) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List this session's pending waits",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := waitSessionID()
			if err != nil {
				return err
			}
			var res listWaitsAPIResponse
			if err := ctx.getJSON(cmd.Context(), waitsPath(session), &res); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			out := cmd.OutOrStdout()
			if len(res.Waits) == 0 {
				_, err := fmt.Fprintln(out, "No pending waits.")
				return err
			}
			now := ctx.deps.Now()
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tWAITING FOR\tSINCE\tWAKES BY\tNOTE")
			for _, w := range res.Waits {
				deadline := w.ExpiresAt
				if w.FireAt != nil {
					deadline = *w.FireAt
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s ago\tin %s\t%s\n", w.ID, waitTarget(w), shortDuration(now.Sub(w.CreatedAt)), shortDuration(deadline.Sub(now)), w.Note)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print waits as JSON")
	return cmd
}

func newWaitCancelCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <wait-id>",
		Short: "Cancel one of this session's pending waits",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := waitSessionID()
			if err != nil {
				return err
			}
			id := strings.TrimSpace(args[0])
			if id == "" {
				return usageError{errors.New("usage: wait id must not be blank")}
			}
			var res waitEnvelopeDTO
			if err := ctx.deleteJSON(cmd.Context(), waitsPath(session)+"/"+url.PathEscape(id), &res); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Cancelled %s (%s).\n", res.Wait.ID, waitTarget(res.Wait))
			return err
		},
	}
}

// waitSessionID is the session this command runs in. Waits always wake the
// session that registered them, so there is no --session flag.
func waitSessionID() (string, error) {
	session := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	if session == "" {
		return "", usageError{errors.New("ao wait must run inside an AO session (AO_SESSION_ID is not set); it wakes the session that registered it")}
	}
	return session, nil
}

func waitsPath(session string) string { return "sessions/" + url.PathEscape(session) + "/waits" }

// parseWaitPR accepts 5807, #5807, or a complete PR/MR URL.
func parseWaitPR(raw string) (int, string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return 0, raw, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(raw, "#"))
	if err != nil || n <= 0 {
		return 0, "", usageError{errors.New("usage: ao wait pr <number|url>: the PR must be a positive number or a PR URL")}
	}
	return n, "", nil
}

// parseWaitDuration is time.ParseDuration plus a whole-day "d" suffix.
func parseWaitDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid duration %q", raw)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 90s, 20m, 1h30m, 2d)", raw)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("invalid duration %q: use whole seconds", raw)
	}
	return d, nil
}

func waitTarget(w waitDTO) string {
	if w.Kind == "time" {
		return "timer"
	}
	switch w.For {
	case "merge":
		return fmt.Sprintf("merge of #%d", w.PRNumber)
	case "review":
		return fmt.Sprintf("review on #%d", w.PRNumber)
	case "any":
		return fmt.Sprintf("CI/review/merge on #%d", w.PRNumber)
	default:
		return fmt.Sprintf("CI on #%d", w.PRNumber)
	}
}

// writeWaitRegistered prints the line agents are prompted to act on: the wait
// id, what AO will report, and the instruction to stop working now.
func writeWaitRegistered(out io.Writer, res createWaitAPIResponse) error {
	w := res.Wait
	var when string
	switch {
	case w.Kind == "time" && w.FireAt != nil:
		when = "in " + shortDuration(w.FireAt.Sub(w.CreatedAt))
	case w.For == "merge":
		when = fmt.Sprintf("when #%d is merged or closed", w.PRNumber)
	case w.For == "review":
		when = fmt.Sprintf("when a new review or review comment arrives on #%d", w.PRNumber)
	case w.For == "any":
		when = fmt.Sprintf("when CI on #%d finishes, a review arrives, or it is merged or closed", w.PRNumber)
	default:
		when = fmt.Sprintf("when CI on #%d finishes", w.PRNumber)
	}
	prefix := "Waiting"
	if res.Existing {
		prefix = "Already waiting"
	}
	_, err := fmt.Fprintf(out, "%s (%s): AO will message this session %s. End your turn now.\n", prefix, w.ID, when)
	return err
}

// shortDuration renders a non-negative duration as e.g. 23h55m, 20m, 45s.
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= time.Hour {
		d = d.Round(time.Minute)
	} else {
		d = d.Round(time.Second)
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
