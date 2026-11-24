// Package agentproto defines the versioned vamos-agent socket RPC envelope,
// capability handshake, durable queue schema, host concurrency policy, and
// principal ACL checks shared by the client, agent, and upgrade workflows.
package agentproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	"github.com/nchapman/vamos/internal/buildmeta"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
)

const (
	APIVersion       = 1
	MinSupportedAPI  = 1
	MaxSupportedAPI  = 1
	MaxEnvelopeBytes = 128 * 1024
)

const (
	CapAppDeploy         = "app-deploy"
	CapProtectedPreview  = "protected-preview"
	CapJobs              = "scheduled-jobs"
	CapHostNetwork       = "service-host-network"
	CapDurableQueue      = "durable-queue"
	CapPerAppConcurrency = "per-app-concurrency"
	CapScopedACL         = "scoped-acl"
	CapSecrets           = "secrets"
	CapBackups           = "backups"
	CapSessions          = "sessions"
)

var (
	hex32RE     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	idempRE     = regexp.MustCompile(`^[0-9a-f]{32,64}$`)
	appLabelRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	principalRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)
	capRE       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

type Method string

const (
	MethodHello          Method = "hello"
	MethodSubmit         Method = "submit"
	MethodRead           Method = "read"
	MethodStatus         Method = "status"
	MethodWait           Method = "wait"
	MethodCancel         Method = "cancel"
	MethodHostStatus     Method = "host-status"
	MethodSession        Method = "session"
	MethodSessionPrepare Method = "session-prepare"
)

type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RPCRequest struct {
	Version int             `json:"version"`
	Method  Method          `json:"method"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type RPCResponse struct {
	Version int             `json:"version"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type HelloRequest struct {
	ClientVersion string `json:"client_version,omitempty"`
	ClientBuildID string `json:"client_build_id,omitempty"`
	APIMin        int    `json:"api_min"`
	APIMax        int    `json:"api_max"`
}

type HelloResponse struct {
	AgentVersion string            `json:"agent_version"`
	AgentBuildID string            `json:"agent_build_id"`
	APIMin       int               `json:"api_min"`
	APIMax       int               `json:"api_max"`
	Capabilities []string          `json:"capabilities"`
	HostID       string            `json:"host_id"`
	HostName     string            `json:"host_name"`
	Domain       string            `json:"domain"`
	Concurrency  ConcurrencyPolicy `json:"concurrency"`
}

// ValidateHelloResponse ensures an agent handshake carries strict version,
// build-identity, and host-identity fields before capability negotiation.
func ValidateHelloResponse(h HelloResponse) error {
	if !buildmeta.ValidLabel(h.AgentVersion) {
		return errors.New("invalid agent version label")
	}
	if !buildmeta.ValidID(h.AgentBuildID) {
		return errors.New("invalid agent build identity")
	}
	if h.APIMin < 1 || h.APIMax < h.APIMin {
		return errors.New("invalid agent API version range")
	}
	if !hex32RE.MatchString(h.HostID) {
		return errors.New("invalid agent host identity")
	}
	if err := lifecycle.ValidateConfig(lifecycle.Config{Name: h.HostName, Domain: h.Domain}); err != nil {
		return fmt.Errorf("invalid agent host config: %w", err)
	}
	if err := h.Concurrency.Validate(); err != nil {
		return fmt.Errorf("invalid agent concurrency policy: %w", err)
	}
	seen := make(map[string]bool, len(h.Capabilities))
	for _, c := range h.Capabilities {
		if !capRE.MatchString(c) || seen[c] {
			return fmt.Errorf("invalid or duplicate agent capability %q", c)
		}
		seen[c] = true
	}
	return nil
}

// CheckCompatibility verifies the client and server agent share a supported
// protocol version and that the agent advertises every required capability.
func CheckCompatibility(clientMin, clientMax int, requiredCaps []string, hello HelloResponse) error {
	if clientMin < 1 || clientMax < clientMin {
		return errors.New("invalid client API range")
	}
	if err := ValidateHelloResponse(hello); err != nil {
		return err
	}
	if clientMax < hello.APIMin {
		return fmt.Errorf("local vamos CLI (API v%d..v%d) is too old for server agent (supports v%d..v%d); upgrade local vamos CLI", clientMin, clientMax, hello.APIMin, hello.APIMax)
	}
	if clientMin > hello.APIMax {
		return fmt.Errorf("server agent (API v%d..v%d) is older than required client minimum v%d; run 'vamos server upgrade'", hello.APIMin, hello.APIMax, clientMin)
	}
	for _, req := range requiredCaps {
		if !slices.Contains(hello.Capabilities, req) {
			return fmt.Errorf("server agent does not advertise required capability %q; run 'vamos server upgrade'", req)
		}
	}
	return nil
}

type ConcurrencyPolicy struct {
	MaxQueueDepth        int   `json:"max_queue_depth"`
	MaxStagedBytes       int64 `json:"max_staged_bytes"`
	MaxConcurrentBuilds  int   `json:"max_concurrent_builds"`
	MaxConcurrentDeploys int   `json:"max_concurrent_deploys"`
}

func DefaultConcurrencyPolicy() ConcurrencyPolicy {
	return ConcurrencyPolicy{
		MaxQueueDepth:        32,
		MaxStagedBytes:       512 << 20,
		MaxConcurrentBuilds:  1,
		MaxConcurrentDeploys: 1,
	}
}

func (p ConcurrencyPolicy) Validate() error {
	if p.MaxQueueDepth < 1 || p.MaxQueueDepth > 256 {
		return errors.New("max_queue_depth must be between 1 and 256")
	}
	if p.MaxStagedBytes < 32<<20 || p.MaxStagedBytes > 16<<30 {
		return errors.New("max_staged_bytes must be between 32 MiB and 16 GiB")
	}
	if p.MaxConcurrentBuilds < 1 || p.MaxConcurrentBuilds > 16 {
		return errors.New("max_concurrent_builds must be between 1 and 16")
	}
	if p.MaxConcurrentDeploys < 1 || p.MaxConcurrentDeploys > 16 {
		return errors.New("max_concurrent_deploys must be between 1 and 16")
	}
	return nil
}

type Role string

const (
	RoleAdmin    Role = "admin"
	RoleDeployer Role = "deployer"
)

type Principal struct {
	ID      string   `json:"id"`
	Role    Role     `json:"role"`
	Apps    []string `json:"apps,omitempty"`
	Actions []string `json:"actions,omitempty"`
}

var hostAdminActions = map[string]bool{
	"server-add":       true,
	"server-upgrade":   true,
	"server-configure": true,
	"server-teardown":  true,
	"acl-manage":       true,
}

var appActions = map[string]bool{
	"apps":              true,
	"deploy":            true,
	"status":            true,
	"logs":              true,
	"run":               true,
	"rollback":          true,
	"resume":            true,
	"cleanup":           true,
	"destroy":           true,
	"service-remove":    true,
	"secrets-stage":     true,
	"secrets-set":       true,
	"secrets-list":      true,
	"backup-plan":       true,
	"backup-repository": true,
	"exec":              true,
}

// Validate checks static principal syntax before persistence or authorization.
func (p Principal) Validate() error {
	if !principalRE.MatchString(p.ID) {
		return errors.New("invalid principal ID")
	}
	if p.Role != RoleAdmin && p.Role != RoleDeployer {
		return fmt.Errorf("unsupported principal role %q", p.Role)
	}
	seenApps := make(map[string]bool, len(p.Apps))
	for _, app := range p.Apps {
		if app != "*" && (len(app) > 63 || !appLabelRE.MatchString(app)) {
			return fmt.Errorf("invalid principal app scope %q", app)
		}
		if seenApps[app] {
			return fmt.Errorf("duplicate principal app scope %q", app)
		}
		seenApps[app] = true
	}
	seenActions := make(map[string]bool, len(p.Actions))
	for _, action := range p.Actions {
		if !appActions[action] && !hostAdminActions[action] {
			return fmt.Errorf("unknown principal action %q", action)
		}
		if p.Role != RoleAdmin && hostAdminActions[action] {
			return fmt.Errorf("principal role %q cannot declare host administration action %q", p.Role, action)
		}
		if seenActions[action] {
			return fmt.Errorf("duplicate principal action %q", action)
		}
		seenActions[action] = true
	}
	return nil
}

// Authorize enforces server-side principal ACLs. It fails closed on unknown
// actions, host-admin operations by non-admins, or out-of-scope applications.
func Authorize(p Principal, action, app string) error {
	if action == "jobs" {
		action = "logs"
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if hostAdminActions[action] {
		if p.Role != RoleAdmin {
			return fmt.Errorf("principal %q is not authorized for host administration action %q", p.ID, action)
		}
		return nil
	}
	if !appActions[action] {
		return fmt.Errorf("unknown action %q", action)
	}
	if len(p.Actions) > 0 && !slices.Contains(p.Actions, action) {
		return fmt.Errorf("principal %q is not authorized for action %q", p.ID, action)
	}
	if p.Role == RoleAdmin && len(p.Apps) == 0 {
		return nil
	}
	if len(p.Apps) == 0 {
		return fmt.Errorf("principal %q has no authorized applications", p.ID)
	}
	if slices.Contains(p.Apps, "*") {
		return nil
	}
	if action == "apps" {
		return fmt.Errorf("principal %q cannot list all host applications without wildcard scope", p.ID)
	}
	if app == "" || !slices.Contains(p.Apps, app) {
		return fmt.Errorf("principal %q is not authorized for app %q", p.ID, app)
	}
	return nil
}

type OperationPhase string

const (
	PhaseQueued    OperationPhase = "queued"
	PhaseRunning   OperationPhase = "running"
	PhaseSucceeded OperationPhase = "succeeded"
	PhaseFailed    OperationPhase = "failed"
	PhaseCancelled OperationPhase = "cancelled"
	PhaseUncertain OperationPhase = "uncertain"
)

func ValidPhase(p OperationPhase) bool {
	switch p {
	case PhaseQueued, PhaseRunning, PhaseSucceeded, PhaseFailed, PhaseCancelled, PhaseUncertain:
		return true
	default:
		return false
	}
}

func CanTransition(from, to OperationPhase) bool {
	switch from {
	case PhaseQueued:
		return to == PhaseRunning || to == PhaseCancelled
	case PhaseRunning:
		return to == PhaseSucceeded || to == PhaseFailed || to == PhaseUncertain
	default:
		return false
	}
}

type SubmitRequest struct {
	IdempotencyKey string         `json:"idempotency_key"`
	Request        deploy.Request `json:"request"`
	// PayloadBytes and PayloadSHA256 describe the bounded binary stream sent
	// immediately after an accepted submit frame. Private values and build
	// contexts never enter the JSON control envelope.
	PayloadBytes  int64  `json:"payload_bytes,omitempty"`
	PayloadSHA256 string `json:"payload_sha256,omitempty"`
}

func ValidateSubmitRequest(r SubmitRequest) error {
	if !idempRE.MatchString(r.IdempotencyKey) {
		return errors.New("idempotency_key must be 32–64 lowercase hex characters")
	}
	if r.PayloadBytes < 0 || r.PayloadBytes > 16<<30 {
		return errors.New("payload_bytes must be between 0 and 16 GiB")
	}
	if (r.PayloadBytes == 0) != (r.PayloadSHA256 == "") || (r.PayloadSHA256 != "" && !buildmeta.ValidID(r.PayloadSHA256)) {
		return errors.New("payload size and SHA-256 must be specified together")
	}
	if err := deploy.ValidateRequest(r.Request); err != nil {
		return err
	}
	return nil
}

type SubmitResponse struct {
	OperationID    string         `json:"operation_id"`
	IdempotencyKey string         `json:"idempotency_key"`
	Phase          OperationPhase `json:"phase"`
	QueuePosition  int            `json:"queue_position"`
	Deduplicated   bool           `json:"deduplicated,omitempty"`
}

// ReadRequest carries one non-mutating app request. Reads never persist queue
// records: they are authorized, executed, and answered directly.
type ReadRequest struct {
	Request deploy.Request `json:"request"`
}

func ValidateReadRequest(r ReadRequest) error {
	if err := deploy.ValidateRequest(r.Request); err != nil {
		return err
	}
	if r.Request.Mutating() {
		return errors.New("read requires a non-mutating action; use submit")
	}
	return nil
}

type WindowSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// SessionEnvelope opens one interactive session. The client-generated token
// names the host staging directory exactly as the direct executor transport
// does; the forced bridge principal still owns authorization.
type SessionEnvelope struct {
	Token   string                `json:"token"`
	Request deploy.SessionRequest `json:"request"`
	Window  *WindowSize           `json:"window,omitempty"`
}

func ValidateSessionEnvelope(s SessionEnvelope) error {
	if !hex32RE.MatchString(s.Token) {
		return errors.New("session token must be 32 lowercase hex characters")
	}
	if !s.Request.TTY && s.Window != nil {
		return errors.New("window size requires a TTY session")
	}
	return deploy.ValidateSessionRequest(s.Request)
}

// SessionExit is the typed control frame that terminates a session relay. It
// cannot be spoofed by workload output because stream data always travels in
// typed chunk frames.
type SessionExit struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

type OperationRecord struct {
	Version        int              `json:"version"`
	OperationID    string           `json:"operation_id"`
	IdempotencyKey string           `json:"idempotency_key"`
	Action         string           `json:"action"`
	App            string           `json:"app,omitempty"`
	Service        string           `json:"service,omitempty"`
	PrincipalID    string           `json:"principal_id"`
	Phase          OperationPhase   `json:"phase"`
	QueuePosition  int              `json:"queue_position"`
	CreatedAt      time.Time        `json:"created_at"`
	StartedAt      *time.Time       `json:"started_at,omitempty"`
	FinishedAt     *time.Time       `json:"finished_at,omitempty"`
	Response       *deploy.Response `json:"response,omitempty"`
	Error          string           `json:"error,omitempty"`
	ErrorCode      string           `json:"error_code,omitempty"`
	ResolvedBy     string           `json:"resolved_by,omitempty"`
}

func ValidateOperationRecord(rec OperationRecord) error {
	if rec.Version != 1 {
		return errors.New("unsupported operation record version")
	}
	if !hex32RE.MatchString(rec.OperationID) {
		return errors.New("invalid operation_id")
	}
	if !idempRE.MatchString(rec.IdempotencyKey) {
		return errors.New("invalid idempotency_key")
	}
	if !appActions[rec.Action] {
		return fmt.Errorf("invalid operation action %q", rec.Action)
	}
	if rec.App != "" && (len(rec.App) > 63 || !appLabelRE.MatchString(rec.App)) {
		return errors.New("invalid operation app name")
	}
	if rec.Service != "" && (len(rec.Service) > 63 || !appLabelRE.MatchString(rec.Service)) {
		return errors.New("invalid operation service name")
	}
	if !principalRE.MatchString(rec.PrincipalID) {
		return errors.New("invalid principal_id")
	}
	if !ValidPhase(rec.Phase) {
		return fmt.Errorf("invalid operation phase %q", rec.Phase)
	}
	if rec.QueuePosition < 0 {
		return errors.New("negative queue_position")
	}
	if rec.CreatedAt.IsZero() {
		return errors.New("created_at is required")
	}
	if rec.ResolvedBy != "" && (rec.Phase != PhaseUncertain || !hex32RE.MatchString(rec.ResolvedBy)) {
		return errors.New("resolved_by requires an uncertain operation and 32-char hex operation_id")
	}
	return nil
}

// EncodeFrame writes a single newline-delimited JSON frame within MaxEnvelopeBytes.
func EncodeFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxEnvelopeBytes {
		return fmt.Errorf("agent frame exceeds %d bytes", MaxEnvelopeBytes)
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// DecodeFrame reads one newline-delimited JSON frame (stopping at '\n' or EOF
// without consuming bytes belonging to subsequent frames), rejects oversized
// lines, unknown fields, and multiple JSON documents on the same line.
func DecodeFrame(r io.Reader, dst any) error {
	var line bytes.Buffer
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			if line.Len() >= MaxEnvelopeBytes {
				return fmt.Errorf("agent frame exceeds %d bytes", MaxEnvelopeBytes)
			}
			line.WriteByte(b[0])
			if b[0] == '\n' {
				break
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	data := line.Bytes()
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("empty agent frame")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode agent frame: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one JSON agent frame")
	}
	return nil
}
