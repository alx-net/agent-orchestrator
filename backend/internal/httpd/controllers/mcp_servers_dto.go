package controllers

import "time"

// ListMCPServersQuery selects how fresh the catalog must be.
type ListMCPServersQuery struct {
	Refresh bool `query:"refresh" description:"Run a new check even over a fresh one."`
	Wait    bool `query:"wait" description:"Block until the check finishes."`
	NoCheck bool `query:"noCheck" description:"Return only what is known, starting nothing."`
}

// SessionMCPServersQuery selects how fresh a session's list must be.
type SessionMCPServersQuery struct {
	Refresh bool `query:"refresh" description:"For a session with no running agent, run a new check."`
}

// MCPServerNameParam names a server.
type MCPServerNameParam struct {
	ServerName string `path:"serverName" description:"MCP server name."`
}

// MCPOperationIDParam names a sign-in.
type MCPOperationIDParam struct {
	OperationID string `path:"operationId" description:"MCP sign-in operation identifier."`
}

// MCPSessionIDParam names a session.
type MCPSessionIDParam struct {
	SessionID string `path:"sessionId" description:"Session identifier."`
}

// MCPSessionServerParams names one server in a session.
type MCPSessionServerParams struct {
	SessionID  string `path:"sessionId" description:"Session identifier."`
	ServerName string `path:"serverName" description:"MCP server name."`
}

// MCPServerAccountStatus is one server on one subscription.
type MCPServerAccountStatus struct {
	AccountID string `json:"accountId"`
	Status    string `json:"status" enum:"connected,needs_sign_in,key_rejected,needs_approval,rejected,failed,starting,disabled,not_checked,unknown" description:"The server's state in AO's words; rawStatus has Claude Code's own."`
	RawStatus string `json:"rawStatus,omitempty" description:"Claude Code's own word for the state, shown for a state AO has no word for."`
	ToolCount *int   `json:"toolCount,omitempty" description:"How many tools a connected server exposes, once known."`
	Error     string `json:"error,omitempty" description:"The last failure, in the server's or Claude Code's own words."`
	// CheckedAt is when this state was read; absent when it never was.
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	Action    string     `json:"action,omitempty" enum:"connect,approve,edit_key,manage" description:"What can fix or manage this server here."`
	Live      bool       `json:"live,omitempty" description:"A running agent reported this, rather than a separate check."`
}

// MCPServerPayload is one server and its state on each subscription.
type MCPServerPayload struct {
	Name        string `json:"name"`
	Transport   string `json:"transport,omitempty" description:"stdio, http, sse, or whatever else Claude Code reports."`
	Target      string `json:"target,omitempty" description:"URL or command, with anything credential-shaped removed."`
	Scope       string `json:"scope" enum:"global,repo,local,connector,plugin,other" description:"Where the server is defined."`
	ProjectID   string `json:"projectId,omitempty"`
	ProjectName string `json:"projectName,omitempty"`
	Auth        string `json:"auth" enum:"oauth,api_key,helper,none,connector,unknown" description:"How the server authenticates."`
	// HeaderNames lists the headers it sends. Values never leave the config.
	HeaderNames []string                 `json:"headerNames,omitempty"`
	Editable    bool                     `json:"editable,omitempty" description:"AO owns the config, so its key can be changed here."`
	ManageURL   string                   `json:"manageUrl,omitempty" description:"Where it is managed, when that is outside AO."`
	Accounts    []MCPServerAccountStatus `json:"accounts"`
}

// MCPAccountRef names a subscription in a list.
type MCPAccountRef struct {
	ID     string `json:"id"`
	Email  string `json:"email,omitempty"`
	Device bool   `json:"device,omitempty"`
}

// MCPServersResponse is the catalog.
type MCPServersResponse struct {
	Servers  []MCPServerPayload `json:"servers"`
	Accounts []MCPAccountRef    `json:"accounts"`
	Checking bool               `json:"checking" description:"A check is running; poll and the list will fill in."`
}

// AddMCPServerRequest adds a server at the user's global scope. Header and env
// values are secrets: AO passes them to Claude Code's config and logs none.
type AddMCPServerRequest struct {
	Name      string            `json:"name"`
	URL       string            `json:"url,omitempty"`
	Transport string            `json:"transport,omitempty" enum:"http,sse" description:"For a URL; http by default."`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// UpdateMCPServerKeyRequest replaces a global server's headers.
type UpdateMCPServerKeyRequest struct {
	Headers map[string]string `json:"headers" description:"Header names and new values. An empty value keeps that header's current one."`
}

// MCPPropagation says how a change reached running agents.
type MCPPropagation struct {
	InPlace    []string          `json:"inPlace,omitempty" description:"Sessions that have it now, with no restart."`
	Restarting []string          `json:"restarting,omitempty" description:"Sessions restarting once idle, conversation kept."`
	Failed     map[string]string `json:"failed,omitempty" description:"Sessions where the server itself failed, with the reason."`
}

// MCPChangeResponse reports a config change.
type MCPChangeResponse struct {
	Applied MCPPropagation `json:"applied"`
}

// MCPServerTargetRequest says whose server an operation is about. With a session,
// it uses that session's account and folder.
type MCPServerTargetRequest struct {
	Name      string `json:"name"`
	SessionID string `json:"sessionId,omitempty"`
	AccountID string `json:"accountId,omitempty" description:"Omitted is this computer's own account."`
	ProjectID string `json:"projectId,omitempty" description:"For a server in that project's .mcp.json."`
}

// MCPLiveStatus is one server in a running agent.
type MCPLiveStatus struct {
	Status    string `json:"status"`
	ToolCount *int   `json:"toolCount,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MCPServerTestResponse is a real check of one server.
type MCPServerTestResponse struct {
	OK        bool           `json:"ok"`
	Name      string         `json:"name"`
	AccountID string         `json:"accountId"`
	Status    string         `json:"status"`
	RawStatus string         `json:"rawStatus,omitempty"`
	Reason    string         `json:"reason" description:"Why it passed, or what is wrong, in plain words."`
	Error     string         `json:"error,omitempty"`
	ToolCount *int           `json:"toolCount,omitempty"`
	Tools     []string       `json:"tools,omitempty"`
	Auth      string         `json:"auth,omitempty"`
	Action    string         `json:"action,omitempty"`
	CheckedAt time.Time      `json:"checkedAt"`
	Live      *MCPLiveStatus `json:"live,omitempty" description:"What the session's running agent has now, for a session's test."`
}

// MCPConnectOperation is a browser sign-in.
type MCPConnectOperation struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"accountId"`
	SessionID string `json:"sessionId,omitempty"`
	State     string `json:"state" enum:"waiting,waiting_external,connected,failed,timed_out,cancelled"`
	// AuthURL is the page to open if the browser did not. It is one-time.
	AuthURL    string         `json:"authUrl,omitempty"`
	Error      string         `json:"error,omitempty"`
	ManageURL  string         `json:"manageUrl,omitempty"`
	Applied    MCPPropagation `json:"applied"`
	StartedAt  time.Time      `json:"startedAt"`
	FinishedAt *time.Time     `json:"finishedAt,omitempty"`
}

// MCPConnectOperationResponse wraps a sign-in.
type MCPConnectOperationResponse struct {
	Operation MCPConnectOperation `json:"operation"`
}

// SessionMCPServersResponse is what one session's agent has.
type SessionMCPServersResponse struct {
	Servers   []MCPServerPayload `json:"servers"`
	AccountID string             `json:"accountId"`
	Live      bool               `json:"live" description:"A running agent reported this. Otherwise it is a check of what the session's account and folder would have."`
}
