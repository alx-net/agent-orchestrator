package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// checkoutAPIRequest and checkoutDTO mirror the daemon's CreateCheckoutRequest
// and CheckoutResponse without importing the controller package.
type checkoutAPIRequest struct {
	Project string `json:"project"`
	Ref     string `json:"ref,omitempty"`
	Refresh bool   `json:"refresh,omitempty"`
}

type checkoutDTO struct {
	ProjectID    string    `json:"projectId"`
	ProjectName  string    `json:"projectName"`
	Path         string    `json:"path"`
	Ref          string    `json:"ref"`
	Commit       string    `json:"commit"`
	ShortCommit  string    `json:"shortCommit"`
	CommitDate   time.Time `json:"commitDate"`
	Subject      string    `json:"subject"`
	Action       string    `json:"action"`
	Note         string    `json:"note,omitempty"`
	FetchWarning string    `json:"fetchWarning,omitempty"`
	Submodules   bool      `json:"submodules,omitempty"`
}

func newCheckoutCommand(ctx *commandContext) *cobra.Command {
	var ref string
	var refresh, jsonOutput bool
	cmd := &cobra.Command{
		Use:   "checkout <project>",
		Short: "Get this session's own up-to-date copy of one of your projects",
		Long: "Fetch the project's remote and create (or refresh) a detached Git worktree of its default branch\n" +
			"inside this session's workspace, at repos/<project-id>. The project is matched by id or name.\n" +
			"Your own checkout of the project is never modified. A checkout with local changes is left as is.\n" +
			"AO removes the session's checkouts when the session is cleaned up.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			session := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
			if session == "" {
				return usageError{errors.New("ao checkout must run inside an AO session (AO_SESSION_ID is not set); the copy goes into that session's workspace")}
			}
			project := strings.TrimSpace(args[0])
			if project == "" {
				return usageError{errors.New("usage: ao checkout <project>: name a project id or name")}
			}
			ref = strings.TrimSpace(ref)
			if strings.HasPrefix(ref, "-") {
				return usageError{fmt.Errorf("usage: --ref %q: use a branch, tag, or commit SHA", ref)}
			}
			var res checkoutDTO
			// No client timeout: the daemon bounds the fetch and checkout, and a
			// large repository's first checkout can outlast the default one.
			path := "/api/v1/sessions/" + url.PathEscape(session) + "/checkouts"
			if err := ctx.doJSONPathWithHeadersAndTimeout(cmd.Context(), http.MethodPost, path, checkoutAPIRequest{Project: project, Ref: ref, Refresh: refresh}, &res, nil, 0); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writeCheckout(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "Check out this remote branch, tag, or commit SHA instead of the default branch")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Fail instead of keeping an existing copy that has local changes, a branch, or unpushed commits")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print the checkout as JSON")
	return cmd
}

func writeCheckout(w io.Writer, res checkoutDTO) error {
	var b strings.Builder
	switch res.Action {
	case "created":
		fmt.Fprintf(&b, "Checked out %s (%s) at %s.\n", res.ProjectName, res.ProjectID, res.Ref)
	case "updated":
		fmt.Fprintf(&b, "Updated %s (%s) to %s.\n", res.ProjectName, res.ProjectID, res.Ref)
	case "unchanged":
		fmt.Fprintf(&b, "%s (%s) is already at the latest %s.\n", res.ProjectName, res.ProjectID, res.Ref)
	default:
		fmt.Fprintf(&b, "Left your copy of %s (%s) untouched because %s; it is not at the latest %s.\n", res.ProjectName, res.ProjectID, res.Note, res.Ref)
	}
	fmt.Fprintf(&b, "path:   %s\n", res.Path)
	date := ""
	if !res.CommitDate.IsZero() {
		date = " " + res.CommitDate.Format("2006-01-02 15:04 -0700")
	}
	fmt.Fprintf(&b, "commit: %s%s %s\n", res.ShortCommit, date, res.Subject)
	if res.FetchWarning != "" {
		fmt.Fprintf(&b, "warning: %s\n", res.FetchWarning)
	}
	if res.Submodules {
		b.WriteString("note: submodules are not initialized; run `git submodule update --init <path>` there if you need one.\n")
	}
	b.WriteString("Work there (cd into the path). It is a detached checkout: run `git switch -c <branch>` before you commit.\n")
	_, err := io.WriteString(w, b.String())
	return err
}
