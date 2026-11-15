package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ReportInterruptWindow is the durable per-worker urgent interruption limit.
const ReportInterruptWindow = 3 * time.Minute

// OrchestratorBusyRetry is how long a batch refused with ErrOrchestratorBusy
// waits before its next delivery attempt.
const OrchestratorBusyRetry = 30 * time.Second

// ErrReportDeliveryModeUnsupported reports a target without semantic delivery.
var ErrReportDeliveryModeUnsupported = errors.New("report delivery requires a semantic conversation")

// ErrOrchestratorBusy marks a Submit refused because the orchestrator cannot
// take the batch right now — its composer holds the operator's unsent draft —
// and should be retried later rather than immediately.
var ErrOrchestratorBusy = errors.New("orchestrator cannot take reports right now")

// DeliveryStore is the durable report scheduling and routing boundary.
type DeliveryStore interface {
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	ListPendingReportSchedule(context.Context, int64) ([]domain.ReportRecord, error)
	ClaimPendingReportBatch(context.Context, domain.ProjectID, string, time.Time) ([]domain.ReportRecord, error)
	AcknowledgeReportBatch(context.Context, domain.ProjectID, string, time.Time) (int, error)
	ReleaseReportBatch(context.Context, domain.ProjectID, string, time.Time, string) (int, error)
	DeferReportBatch(context.Context, domain.ProjectID, string, time.Time, string) (int, error)
	AcquireReportInterrupt(context.Context, domain.SessionID, time.Time, time.Duration) (bool, time.Time, error)
}

// FYIStore is the durable `ao send --fyi` outbox. It is optional: without it
// no FYI is claimed and report delivery is unchanged.
type FYIStore interface {
	ClaimPendingFYIMessages(context.Context, domain.SessionID, string, time.Time) ([]domain.FYIMessage, error)
	AcknowledgeFYIMessages(context.Context, domain.SessionID, string, time.Time) (int, error)
	ReleaseFYIMessages(context.Context, domain.SessionID, string) (int, error)
}

// Delivery submits accepted semantic context and interrupts active work.
type Delivery interface {
	Submit(context.Context, domain.SessionID, string, string) error
	Interrupt(context.Context, domain.SessionID) error
}

// Coordinator delivers durable report batches to active orchestrators.
type Coordinator struct {
	store      DeliveryStore
	fyis       FYIStore
	delivery   Delivery
	now        func() time.Time
	newToken   func() string
	retryDelay time.Duration
	wake       chan struct{}
	mu         sync.Mutex
}

// CoordinatorDeps configures durable scheduling and delivery.
type CoordinatorDeps struct {
	Store DeliveryStore
	// FYIs holds `ao send --fyi` messages for each session's next turn.
	FYIs       FYIStore
	Delivery   Delivery
	Now        func() time.Time
	NewToken   func() string
	RetryDelay time.Duration
}

// NewCoordinator constructs an idle report delivery coordinator.
func NewCoordinator(d CoordinatorDeps) *Coordinator {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.NewToken == nil {
		d.NewToken = uuid.NewString
	}
	if d.RetryDelay <= 0 {
		d.RetryDelay = 5 * time.Second
	}
	return &Coordinator{store: d.Store, fyis: d.FYIs, delivery: d.Delivery, now: d.Now, newToken: d.NewToken, retryDelay: d.RetryDelay, wake: make(chan struct{}, 1)}
}

// Wake asks the coordinator to inspect durable scheduling state.
func (c *Coordinator) Wake() {
	if c == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Start runs delivery until ctx is cancelled and returns its completion signal.
func (c *Coordinator) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); c.run(ctx) }()
	return done
}

func (c *Coordinator) run(ctx context.Context) {
	for {
		delay, err := c.RunDue(ctx)
		if err != nil {
			delay = c.retryDelay
		}
		if delay < 0 {
			delay = time.Hour
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-c.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

// RunDue performs bounded due delivery and returns the delay until the next
// durable deadline. Tests call it directly with fake time.
func (c *Coordinator) RunDue(ctx context.Context) (time.Duration, error) {
	if c == nil || c.store == nil || c.delivery == nil {
		return -1, errors.New("report coordinator dependencies are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		pending, err := c.store.ListPendingReportSchedule(ctx, 1)
		if err != nil {
			return c.retryDelay, err
		}
		if len(pending) == 0 {
			return -1, nil
		}
		now := c.now().UTC()
		if pending[0].AvailableAt.After(now) {
			return pending[0].AvailableAt.Sub(now), nil
		}
		if err := c.deliverProject(ctx, pending[0].ProjectID); err != nil {
			return c.retryDelay, err
		}
	}
}

func (c *Coordinator) deliverProject(ctx context.Context, projectID domain.ProjectID) error {
	target, ok, err := c.activeOrchestrator(ctx, projectID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("project %s has no active orchestrator", projectID)
	}
	batch, err := c.claim(ctx, projectID)
	if err != nil || len(batch.Reports) == 0 {
		return err
	}
	if batch.hasState(domain.ReportStuck) {
		interrupted := false
		var nextInterrupt time.Time
		for _, worker := range batch.stuckWorkers() {
			acquired, next, acquireErr := c.store.AcquireReportInterrupt(ctx, worker, c.now().UTC(), ReportInterruptWindow)
			if acquireErr != nil {
				return c.release(ctx, batch, acquireErr)
			}
			if acquired {
				interrupted = true
				if interruptErr := c.delivery.Interrupt(ctx, target.ID); interruptErr != nil {
					return c.release(ctx, batch, interruptErr)
				}
			} else if nextInterrupt.IsZero() || next.Before(nextInterrupt) {
				nextInterrupt = next
			}
		}
		if !interrupted && !batch.hasState(domain.ReportNeedsInput) {
			_, deferErr := c.store.DeferReportBatch(ctx, batch.ProjectID, batch.Token, nextInterrupt, "repeated stuck report coalesced inside interrupt window")
			return deferErr
		}
	}
	if err := c.delivery.Submit(ctx, target.ID, batch.Render(), batch.IdempotencyKey()); err != nil {
		if errors.Is(err, ErrOrchestratorBusy) {
			// Released, the batch would stay the earliest due and every retry would
			// stop RunDue before any other project's reports. Back off instead.
			_, deferErr := c.store.DeferReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC().Add(OrchestratorBusyRetry), err.Error())
			return deferErr
		}
		return c.release(ctx, batch, err)
	}
	_, err = c.store.AcknowledgeReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC())
	return err
}

func (c *Coordinator) activeOrchestrator(ctx context.Context, projectID domain.ProjectID) (domain.SessionRecord, bool, error) {
	recs, err := c.store.ListSessions(ctx, projectID)
	if err != nil {
		return domain.SessionRecord{}, false, err
	}
	var selected domain.SessionRecord
	for _, rec := range recs {
		if rec.Kind != domain.KindOrchestrator || rec.IsTerminated || rec.Activity.State == domain.ActivityExited {
			continue
		}
		if selected.ID == "" || rec.CreatedAt.After(selected.CreatedAt) {
			selected = rec
		}
	}
	return selected, selected.ID != "", nil
}

// PreparedBatch is a token-fenced durable batch awaiting acceptance.
type PreparedBatch struct {
	ProjectID domain.ProjectID
	Token     string
	ID        string
	Reports   []domain.ReportRecord
	// Target owns FYIs, the `ao send --fyi` messages claimed for its next turn
	// under the same Token.
	Target domain.SessionID
	FYIs   []domain.FYIMessage
}

func (c *Coordinator) claim(ctx context.Context, projectID domain.ProjectID) (PreparedBatch, error) {
	token := c.newToken()
	reports, err := c.store.ClaimPendingReportBatch(ctx, projectID, token, c.now().UTC())
	for i := range reports {
		session, found, lookupErr := c.store.GetSession(ctx, reports[i].SessionID)
		if lookupErr == nil && found {
			reports[i].SessionDisplayName = strings.TrimSpace(session.DisplayName)
		}
	}
	batchID := ""
	if len(reports) > 0 {
		batchID = reports[0].DeliveryBatchID
	}
	return PreparedBatch{ProjectID: projectID, Token: token, ID: batchID, Reports: reports}, err
}

// PreparePiggyback claims pending reports for the active Chat orchestrator,
// and the target's pending FYIs for any session.
// The caller must acknowledge only after its user message is durably accepted.
func (c *Coordinator) PreparePiggyback(ctx context.Context, target domain.SessionID) (PreparedBatch, error) {
	if c == nil || c.store == nil {
		return PreparedBatch{}, nil
	}
	rec, ok, err := c.store.GetSession(ctx, target)
	if err != nil || !ok {
		return PreparedBatch{}, err
	}
	if rec.Kind != domain.KindOrchestrator || rec.IsTerminated || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat {
		return c.PrepareFYIs(ctx, target)
	}
	batch, err := c.claim(ctx, rec.ProjectID)
	if err != nil {
		return batch, err
	}
	fyis, err := c.claimFYIs(ctx, target, batch.Token)
	if err != nil {
		return PreparedBatch{}, errors.Join(err, c.ReleasePiggyback(ctx, batch, err))
	}
	batch.Target, batch.FYIs = target, fyis
	return batch, nil
}

// PrepareFYIs claims the `ao send --fyi` messages waiting for target's next
// turn, whatever starts it. The caller appends the batch to that turn's message
// and must acknowledge only after the turn is durably accepted.
func (c *Coordinator) PrepareFYIs(ctx context.Context, target domain.SessionID) (PreparedBatch, error) {
	if c == nil || c.fyis == nil {
		return PreparedBatch{}, nil
	}
	token := c.newToken()
	fyis, err := c.claimFYIs(ctx, target, token)
	if err != nil || len(fyis) == 0 {
		return PreparedBatch{}, err
	}
	return PreparedBatch{Token: token, Target: target, FYIs: fyis}, nil
}

func (c *Coordinator) claimFYIs(ctx context.Context, target domain.SessionID, token string) ([]domain.FYIMessage, error) {
	if c.fyis == nil || target == "" {
		return nil, nil
	}
	fyis, err := c.fyis.ClaimPendingFYIMessages(ctx, target, token, c.now().UTC())
	if err != nil {
		return nil, fmt.Errorf("claim fyi messages: %w", err)
	}
	for i := range fyis {
		if fyis[i].SenderSessionID == "" || c.store == nil {
			continue
		}
		if sender, found, lookupErr := c.store.GetSession(ctx, fyis[i].SenderSessionID); lookupErr == nil && found {
			fyis[i].SenderDisplayName = strings.TrimSpace(sender.DisplayName)
			fyis[i].SenderProjectID = sender.ProjectID
		}
	}
	return fyis, nil
}

// AcceptPiggyback acknowledges reports and FYIs after the combined user turn is accepted.
func (c *Coordinator) AcceptPiggyback(ctx context.Context, batch PreparedBatch) error {
	var errs []error
	if len(batch.Reports) > 0 {
		if _, err := c.store.AcknowledgeReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC()); err != nil {
			errs = append(errs, err)
		}
	}
	if len(batch.FYIs) > 0 && c.fyis != nil {
		if _, err := c.fyis.AcknowledgeFYIMessages(ctx, batch.Target, batch.Token, c.now().UTC()); err != nil {
			errs = append(errs, fmt.Errorf("acknowledge fyi messages: %w", err))
		}
	}
	return errors.Join(errs...)
}

// ReleasePiggyback makes a batch retryable when the combined turn was refused.
// Released FYIs wait for the target's next turn again.
func (c *Coordinator) ReleasePiggyback(ctx context.Context, batch PreparedBatch, cause error) error {
	var fyiErr error
	if len(batch.FYIs) > 0 && c.fyis != nil {
		if _, err := c.fyis.ReleaseFYIMessages(ctx, batch.Target, batch.Token); err != nil {
			fyiErr = fmt.Errorf("release fyi messages: %w", err)
		}
	}
	if len(batch.Reports) == 0 {
		return fyiErr
	}
	return errors.Join(c.release(ctx, batch, cause), fyiErr)
}

func (c *Coordinator) release(ctx context.Context, batch PreparedBatch, cause error) error {
	_, releaseErr := c.store.ReleaseReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC(), cause.Error())
	if releaseErr != nil {
		return errors.Join(cause, releaseErr)
	}
	return cause
}

const (
	chatReportOpen  = "<ao-worker-reports>"
	chatReportClose = "</ao-worker-reports>"
)

// AppendToUserMessage places reports and FYIs after the user's text in one
// semantic turn. The envelope lets AO's Chat UI hide daemon-authored context
// while preserving the exact prompt accepted by the provider for retry and
// recovery. A standalone report delivery already is that list, so its FYIs
// join it as further entries.
func (b PreparedBatch) AppendToUserMessage(message string) string {
	if len(b.Reports) == 0 && len(b.FYIs) == 0 {
		return message
	}
	if len(b.Reports) == 0 && strings.HasPrefix(message, reportsHeader) {
		var out strings.Builder
		out.WriteString(message)
		b.renderFYIs(&out)
		return out.String()
	}
	return message + "\n\n" + chatReportOpen + "\n" + b.Render() + "\n" + chatReportClose
}

// IdempotencyKey returns the stable durable delivery identity.
func (b PreparedBatch) IdempotencyKey() string {
	if b.ID != "" {
		return b.ID
	}
	ids := make([]string, len(b.Reports))
	for i := range b.Reports {
		ids[i] = b.Reports[i].ID
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return "report-batch:" + hex.EncodeToString(sum[:16])
}

// reportsHeader opens every rendered batch. AO's Chat UI parses entries after it.
const reportsHeader = "Reports since your previous turn:"

// Render formats compact attributed report context for an orchestrator, then
// any FYIs in arrival order.
func (b PreparedBatch) Render() string {
	var out strings.Builder
	out.WriteString(reportsHeader)
	for _, report := range coalesceReports(b.Reports) {
		state := string(report.State)
		if state == "" {
			state = "information"
		}
		fmt.Fprintf(&out, "\n\n[%s] %s", state, reportSessionLink(report))
		if report.RepeatCount > 1 {
			fmt.Fprintf(&out, " (repeated %d times)", report.RepeatCount)
		}
		text := report.Note
		if text == "" {
			text = report.Message
		}
		if text != "" {
			out.WriteString("\n")
			out.WriteString(text)
		}
		for _, output := range report.Outputs {
			fmt.Fprintf(&out, "\n%s: %s", output.Kind, output.Reference)
		}
	}
	b.renderFYIs(&out)
	return out.String()
}

// renderFYIs writes each FYI as a `[fyi]` entry: sender and the time it was
// sent, then its text. The label tells the recipient no reply is expected.
func (b PreparedBatch) renderFYIs(out *strings.Builder) {
	for _, fyi := range b.FYIs {
		fmt.Fprintf(out, "\n\n[fyi] %s (sent %s; informational, no reply needed)\n%s",
			fyiSenderLink(fyi), fyi.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"), fyi.Message)
	}
}

// fyiSenderLink attributes an FYI like a report: a session link when AO knows
// the sender, otherwise its id, or the user when none was given.
func fyiSenderLink(fyi domain.FYIMessage) string {
	if fyi.SenderSessionID == "" {
		return "the user"
	}
	if fyi.SenderProjectID == "" {
		return string(fyi.SenderSessionID)
	}
	return reportSessionLink(domain.ReportRecord{
		SessionID: fyi.SenderSessionID, ProjectID: fyi.SenderProjectID, SessionDisplayName: fyi.SenderDisplayName,
	})
}

func reportDisplayNameOrID(r domain.ReportRecord) string {
	if name := strings.TrimSpace(r.SessionDisplayName); name != "" {
		return name
	}
	return string(r.SessionID)
}

// reportSessionLink is the markdown attribution a standalone report shows.
// Piggybacked copies stay inside the hidden user-message envelope.
func reportSessionLink(r domain.ReportRecord) string {
	label := strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "\n", " ").Replace(reportDisplayNameOrID(r))
	href := "ao://sessions/" + url.PathEscape(string(r.ProjectID)) + "/" + url.PathEscape(string(r.SessionID))
	return "[" + label + "](" + href + ")"
}

func (b PreparedBatch) hasState(state domain.ReportState) bool {
	for _, report := range b.Reports {
		if report.State == state {
			return true
		}
	}
	return false
}

func (b PreparedBatch) stuckWorkers() []domain.SessionID {
	seen := map[domain.SessionID]bool{}
	var out []domain.SessionID
	for _, report := range b.Reports {
		if report.State == domain.ReportStuck && !seen[report.SessionID] {
			seen[report.SessionID] = true
			out = append(out, report.SessionID)
		}
	}
	return out
}

func coalesceReports(in []domain.ReportRecord) []domain.ReportRecord {
	out := make([]domain.ReportRecord, 0, len(in))
	indexes := map[string]int{}
	for _, report := range in {
		key := reportKey(report)
		if report.State == domain.ReportStuck {
			if i, ok := indexes[key]; ok {
				out[i].RepeatCount += report.RepeatCount
				continue
			}
			indexes[key] = len(out)
		}
		out = append(out, report)
	}
	return out
}

func reportKey(report domain.ReportRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%s", report.SessionID, report.State, report.Note, report.Message)
	for _, output := range report.Outputs {
		fmt.Fprintf(&b, "\x00%s\x00%s\x00%s", output.Kind, output.Reference, output.Label)
	}
	return b.String()
}
