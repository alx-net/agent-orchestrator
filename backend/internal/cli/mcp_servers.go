package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The MCP server DTOs mirror the daemon's without importing the controller package.
type mcpServerStatusDTO struct {
	AccountID string     `json:"accountId"`
	Status    string     `json:"status"`
	RawStatus string     `json:"rawStatus,omitempty"`
	ToolCount *int       `json:"toolCount,omitempty"`
	Error     string     `json:"error,omitempty"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	Action    string     `json:"action,omitempty"`
	Live      bool       `json:"live,omitempty"`
}

type mcpServerDTO struct {
	Name        string               `json:"name"`
	Transport   string               `json:"transport,omitempty"`
	Target      string               `json:"target,omitempty"`
	Scope       string               `json:"scope"`
	ProjectName string               `json:"projectName,omitempty"`
	Auth        string               `json:"auth"`
	ManageURL   string               `json:"manageUrl,omitempty"`
	Accounts    []mcpServerStatusDTO `json:"accounts"`
}

type mcpAccountDTO struct {
	ID     string `json:"id"`
	Email  string `json:"email,omitempty"`
	Device bool   `json:"device,omitempty"`
}

type mcpServersDTO struct {
	Servers  []mcpServerDTO  `json:"servers"`
	Accounts []mcpAccountDTO `json:"accounts,omitempty"`
	Live     bool            `json:"live,omitempty"`
	Checking bool            `json:"checking,omitempty"`
}

type mcpAddRequest struct {
	Name      string            `json:"name"`
	URL       string            `json:"url,omitempty"`
	Transport string            `json:"transport,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type mcpTargetRequest struct {
	Name      string `json:"name"`
	SessionID string `json:"sessionId,omitempty"`
	AccountID string `json:"accountId,omitempty"`
}

type mcpPropagationDTO struct {
	InPlace    []string          `json:"inPlace,omitempty"`
	Restarting []string          `json:"restarting,omitempty"`
	Failed     map[string]string `json:"failed,omitempty"`
}

type mcpChangeDTO struct {
	Applied mcpPropagationDTO `json:"applied"`
}

type mcpOperationDTO struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	AccountID string            `json:"accountId"`
	State     string            `json:"state"`
	AuthURL   string            `json:"authUrl,omitempty"`
	Error     string            `json:"error,omitempty"`
	ManageURL string            `json:"manageUrl,omitempty"`
	Applied   mcpPropagationDTO `json:"applied"`
}

type mcpOperationEnvelope struct {
	Operation mcpOperationDTO `json:"operation"`
}

type mcpLiveDTO struct {
	Status    string `json:"status"`
	ToolCount *int   `json:"toolCount,omitempty"`
	Error     string `json:"error,omitempty"`
}

type mcpTestDTO struct {
	OK        bool        `json:"ok"`
	Name      string      `json:"name"`
	AccountID string      `json:"accountId"`
	Status    string      `json:"status"`
	RawStatus string      `json:"rawStatus,omitempty"`
	Reason    string      `json:"reason"`
	Error     string      `json:"error,omitempty"`
	ToolCount *int        `json:"toolCount,omitempty"`
	Tools     []string    `json:"tools,omitempty"`
	Auth      string      `json:"auth,omitempty"`
	Action    string      `json:"action,omitempty"`
	Live      *mcpLiveDTO `json:"live,omitempty"`
}

// mcpLoginPoll is how often `ao mcp login` asks whether the browser finished.
var mcpLoginPoll = time.Second

func mcpSession() string { return strings.TrimSpace(os.Getenv("AO_SESSION_ID")) }

func addMCPServerCommands(ctx *commandContext, parent *cobra.Command) {
	parent.AddCommand(
		newMCPListCommand(ctx), newMCPLoginCommand(ctx), newMCPTestCommand(ctx),
		newMCPAddCommand(ctx), newMCPRemoveCommand(ctx),
	)
}

func newMCPListCommand(ctx *commandContext) *cobra.Command {
	var jsonOutput, refresh bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List MCP servers and their state: inside a session, what its agent has right now",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var path string
			if session := mcpSession(); session != "" {
				path = "sessions/" + url.PathEscape(session) + "/mcp-servers"
				if refresh {
					path += "?refresh=true"
				}
			} else {
				path = "mcp/servers?wait=true"
				if refresh {
					path += "&refresh=true"
				}
			}
			var out mcpServersDTO
			// No request limit: a check starts every server, which takes a while.
			if err := ctx.doJSONPathWithHeadersAndTimeout(cmd.Context(), http.MethodGet, "/api/v1/"+path, nil, &out, nil, 0); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), out)
			}
			return writeMCPServers(cmd.OutOrStdout(), out)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print as JSON")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Run a new check instead of using a recent one")
	return cmd
}

func writeMCPServers(out io.Writer, list mcpServersDTO) error {
	if len(list.Servers) == 0 {
		_, err := fmt.Fprintln(out, "No MCP servers. Add one: ao mcp add <name> <url-or-command>")
		return err
	}
	labels := map[string]string{}
	for _, account := range list.Accounts {
		switch {
		case account.Device:
			labels[account.ID] = "this computer"
		case account.Email != "":
			labels[account.ID] = account.Email
		}
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSTATUS\tTOOLS\tAUTH\tDEFINED\tACCOUNT\tDETAIL")
	for _, server := range list.Servers {
		defined := server.Scope
		if server.ProjectName != "" {
			defined += ":" + server.ProjectName
		}
		for _, status := range server.Accounts {
			tools := "-"
			if status.ToolCount != nil {
				tools = fmt.Sprintf("%d", *status.ToolCount)
			}
			account := labels[status.AccountID]
			if account == "" {
				account = status.AccountID
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", server.Name, mcpStatusWords(status.Status, status.RawStatus), tools, server.Auth, defined, account, mcpDetail(server, status))
		}
	}
	return tw.Flush()
}

// mcpDetail is what to read or do next, on one line.
func mcpDetail(server mcpServerDTO, status mcpServerStatusDTO) string {
	switch {
	case status.Error != "":
		return firstLine(status.Error)
	case status.Action == "connect":
		return "ao mcp login " + server.Name
	case status.Action == "approve":
		return "approve in Settings, MCP servers"
	case status.Action == "manage" && status.Status != "connected":
		return server.ManageURL
	}
	return ""
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

func mcpStatusWords(status, raw string) string {
	switch status {
	case "connected":
		return "connected"
	case "needs_sign_in":
		return "needs sign-in"
	case "key_rejected":
		return "key rejected"
	case "needs_approval":
		return "needs approval"
	case "rejected":
		return "rejected"
	case "failed":
		return "failed"
	case "starting":
		return "starting"
	case "disabled":
		return "disabled"
	case "not_checked":
		return "not checked"
	}
	if raw != "" {
		return raw
	}
	return status
}

func newMCPLoginCommand(ctx *commandContext) *cobra.Command {
	var accountID string
	cmd := &cobra.Command{
		Use:   "login <server>",
		Short: "Connect an MCP server: opens the browser, waits for the approval, then gives this agent its tools",
		Long: "Sign this session's subscription in to an MCP server. The browser opens by itself and the only\n" +
			"thing left is the user's approval there. It blocks until that finishes, then the session's agent\n" +
			"has the server's tools with no restart. Outside a session it signs in this computer's own account,\n" +
			"or the one --account names.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.mcpLogin(cmd.Context(), cmd.OutOrStdout(), args[0], accountID)
		},
	}
	cmd.Flags().StringVar(&accountID, "account", "", "Account id, when not running inside a session")
	return cmd
}

func mcpTargetFor(name, accountID string) mcpTargetRequest {
	target := mcpTargetRequest{Name: name, AccountID: accountID}
	if accountID == "" {
		target.SessionID = mcpSession()
	}
	return target
}

func (c *commandContext) mcpLogin(ctx context.Context, out io.Writer, name, accountID string) error {
	var started mcpOperationEnvelope
	if err := c.postJSON(ctx, "mcp/connect", mcpTargetFor(name, accountID), &started); err != nil {
		return err
	}
	op := started.Operation
	path := "mcp/connect/" + url.PathEscape(op.ID)
	printedURL, printedHandoff := false, false
	_, _ = fmt.Fprintf(out, "Approve it in the browser that just opened. Waiting for %s...\n", name)
	ticker := time.NewTicker(mcpLoginPoll)
	defer ticker.Stop()
	for {
		if op.AuthURL != "" && !printedURL {
			printedURL = true
			_, _ = fmt.Fprintf(out, "If no browser opened, ask the user to open:\n  %s\n", op.AuthURL)
		}
		switch op.State {
		case "connected":
			return writeMCPConnected(out, op)
		case "failed", "timed_out", "cancelled":
			if op.Error != "" {
				return errors.New(op.Error)
			}
			return fmt.Errorf("the sign-in to %s was %s", name, strings.ReplaceAll(op.State, "_", " "))
		case "waiting_external":
			if !printedHandoff {
				printedHandoff = true
				_, _ = fmt.Fprintf(out, "Finish on claude.ai. This keeps waiting and connects when you do; manage connectors at %s\n", op.ManageURL)
			}
		case "waiting":
		default:
			return fmt.Errorf("the sign-in to %s reports %q, which this version of ao does not know", name, op.State)
		}
		select {
		case <-ctx.Done():
			// Stop the sign-in too, so it does not wait 5 minutes on a closed browser.
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = c.postJSON(stopCtx, path+"/cancel", struct{}{}, nil)
			return ctx.Err()
		case <-ticker.C:
		}
		var current mcpOperationEnvelope
		if err := c.getJSON(ctx, path, &current); err != nil {
			return err
		}
		op = current.Operation
	}
}

func writeMCPConnected(out io.Writer, op mcpOperationDTO) error {
	session := mcpSession()
	message := fmt.Sprintf("Connected %s.", op.Name)
	switch {
	case session == "":
	case contains(op.Applied.InPlace, session):
		message += " Its tools are available to you now."
	case contains(op.Applied.Restarting, session):
		message += " AO restarts this agent, conversation kept, as soon as this turn ends; its tools load then."
	default:
		if reason := op.Applied.Failed[session]; reason != "" {
			message += " It did not start here: " + reason
		}
	}
	_, err := fmt.Fprintln(out, message)
	return err
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func newMCPTestCommand(ctx *commandContext) *cobra.Command {
	var accountID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "test <server>",
		Short: "Really connect to one MCP server and list its tools; exits 1 unless it passes",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result mcpTestDTO
			if err := ctx.postJSON(cmd.Context(), "mcp/test", mcpTargetFor(args[0], accountID), &result); err != nil {
				return err
			}
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
					return err
				}
			} else if err := writeMCPTest(cmd.OutOrStdout(), result); err != nil {
				return err
			}
			if !result.OK {
				return mcpTestFailed{}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&accountID, "account", "", "Account id, when not running inside a session")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print as JSON")
	return cmd
}

// mcpTestFailed exits 1: the result itself is already printed.
type mcpTestFailed struct{}

func (mcpTestFailed) Error() string { return "the MCP server did not pass" }

func writeMCPTest(out io.Writer, result mcpTestDTO) error {
	word := "FAIL"
	if result.OK {
		word = "PASS"
	}
	if _, err := fmt.Fprintf(out, "%s %s: %s\n", word, result.Name, result.Reason); err != nil {
		return err
	}
	if len(result.Tools) > 0 {
		if _, err := fmt.Fprintf(out, "Tools: %s\n", strings.Join(result.Tools, ", ")); err != nil {
			return err
		}
	}
	if result.Live != nil {
		line := "This agent has it: " + mcpStatusWords(result.Live.Status, "")
		if result.Live.ToolCount != nil {
			line += fmt.Sprintf(", %d tools", *result.Live.ToolCount)
		}
		if result.Live.Error != "" {
			line += " (" + firstLine(result.Live.Error) + ")"
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func newMCPAddCommand(ctx *commandContext) *cobra.Command {
	var headers, env []string
	var transport string
	cmd := &cobra.Command{
		Use:   "add <name> <url-or-command> [args...]",
		Short: "Add an MCP server to this computer's Claude Code config, for every session",
		Long: "Add an MCP server by URL or by the command that starts it. Running agents get it right away.\n\n" +
			"  ao mcp add linear https://mcp.linear.app/mcp\n" +
			"  ao mcp add acme https://mcp.acme.dev --header \"X-Api-Key: abc123\"\n" +
			"  ao mcp add files --env ROOT=/data -- npx -y @acme/files-mcp\n\n" +
			"Then `ao mcp login <name>` signs in, if the server wants that, and `ao mcp test <name>` checks it.",
		Args: usageArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := mcpAddRequest{Name: args[0], Transport: transport}
			target := args[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
				if len(args) > 2 {
					return usageError{errors.New("usage: a URL takes no further arguments")}
				}
				req.URL = target
			} else {
				if len(headers) > 0 {
					return usageError{errors.New("usage: --header is for a URL; a command takes --env")}
				}
				req.Command, req.Args = target, args[2:]
			}
			var err error
			if req.Headers, err = parseMCPPairs(headers, ":", "--header \"Name: value\""); err != nil {
				return err
			}
			if req.Env, err = parseMCPPairs(env, "=", "--env NAME=value"); err != nil {
				return err
			}
			var result mcpChangeDTO
			if err := ctx.postJSON(cmd.Context(), "mcp/servers", req, &result); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Added %s. %s\nNext: `ao mcp login %s` if it needs a sign-in, then `ao mcp test %s`.\n", req.Name, mcpAppliedWords(result.Applied), req.Name, req.Name)
			return err
		},
	}
	cmd.Flags().StringArrayVarP(&headers, "header", "H", nil, "Header sent to a URL server, \"Name: value\"; repeatable")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "Environment value for a command server, NAME=value; repeatable")
	cmd.Flags().StringVar(&transport, "transport", "", "For a URL: http (default) or sse")
	return cmd
}

func parseMCPPairs(values []string, separator, form string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, separator)
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok || key == "" {
			// The value is a secret: the message must not repeat it.
			return nil, usageError{fmt.Errorf("usage: expected %s", form)}
		}
		out[key] = val
	}
	return out, nil
}

func mcpAppliedWords(applied mcpPropagationDTO) string {
	inPlace, restarting := len(applied.InPlace), len(applied.Restarting)
	switch {
	case inPlace == 0 && restarting == 0:
		return "New sessions get the change."
	case restarting == 0:
		return fmt.Sprintf("Running agents have the change now (%d).", inPlace)
	}
	return fmt.Sprintf("Running agents: %d have the change now, %d restart with their conversation kept once idle.", inPlace, restarting)
}

func newMCPRemoveCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove an MCP server from this computer's Claude Code config",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result mcpChangeDTO
			if err := ctx.deleteJSON(cmd.Context(), "mcp/servers/"+url.PathEscape(args[0]), &result); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed %s. %s\n", args[0], mcpAppliedWords(result.Applied))
			return err
		},
	}
}
