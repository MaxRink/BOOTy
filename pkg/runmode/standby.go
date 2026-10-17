//go:build linux

package runmode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/rescue"
)

const (
	ackTimeout        = 10 * time.Second
	heartbeatInterval = 30 * time.Second
	pollInterval      = 10 * time.Second
)

// StandbyMode keeps the machine in a hot-standby loop, sending periodic
// heartbeats and polling for commands.
type StandbyMode struct {
	deps             Deps
	newDeprovisioner func(*config.MachineConfig) standbyDeprovisioner
}

type standbyDeprovisioner interface {
	Deprovision(context.Context) error
}

// skippedDiskReporter is the optional deprovisioner capability that names
// the disks in use that a wipe of all disks skipped.
type skippedDiskReporter interface {
	SkippedDisks() []string
}

// eraseFallbackReporter is the optional deprovisioner capability that names
// the disks that got the plain wipe instead of the secure erase.
type eraseFallbackReporter interface {
	SecureEraseFallbacks() []string
}

// The orchestrator must keep the capabilities, or the ACK loses the notes.
var (
	_ skippedDiskReporter   = (*provision.Orchestrator)(nil)
	_ eraseFallbackReporter = (*provision.Orchestrator)(nil)
)

// Name returns the mode identifier.
func (m *StandbyMode) Name() string { return "standby" }

// Run enters the standby heartbeat/command polling loop until the context is canceled.
func (m *StandbyMode) Run(ctx context.Context) error {
	if err := m.validateConfig(); err != nil {
		return err
	}

	slog.Info("entering standby mode")
	if err := m.deps.Client.ReportStatus(ctx, config.StatusInit, "standby"); err != nil {
		return fmt.Errorf("standby status readiness: %w", err)
	}
	if err := m.deps.Client.Heartbeat(ctx); err != nil {
		return fmt.Errorf("standby heartbeat readiness: %w", err)
	}
	if result, handled, err := m.pollCommands(ctx); err != nil {
		return fmt.Errorf("standby command readiness: %w", err)
	} else if handled {
		return result.err
	}

	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("standby context canceled, shutting down")
			return fmt.Errorf("standby canceled: %w", ctx.Err())

		case <-heartbeatTicker.C:
			if err := m.deps.Client.Heartbeat(ctx); err != nil {
				slog.Warn("heartbeat failed", "error", err)
			}

		case <-pollTicker.C:
			result, handled, err := m.pollCommands(ctx)
			if err != nil {
				slog.Warn("command poll failed", "error", err)
				continue
			}
			if handled {
				return result.err
			}
		}
	}
}

func (m *StandbyMode) validateConfig() error {
	if m.deps.Cfg == nil {
		return fmt.Errorf("standby mode requires machine config")
	}
	if m.deps.Client == nil {
		return fmt.Errorf("standby mode requires CAPRF client")
	}

	var missing []string
	if strings.TrimSpace(m.deps.Cfg.Agent.HeartbeatURL) == "" {
		missing = append(missing, "HEARTBEAT_URL")
	}
	if strings.TrimSpace(m.deps.Cfg.Agent.CommandsURL) == "" {
		missing = append(missing, "COMMANDS_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("standby mode requires %s", strings.Join(missing, " and "))
	}
	return nil
}

func (m *StandbyMode) pollCommands(ctx context.Context) (*commandResult, bool, error) {
	cmds, err := m.deps.Client.FetchCommands(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, cmd := range cmds {
		result := m.handleCommand(ctx, cmd)
		if result != nil {
			return result, true, nil
		}
	}
	return nil, false, nil
}

type commandResult struct {
	err error
}

func (m *StandbyMode) handleCommand(ctx context.Context, cmd config.Command) *commandResult {
	slog.Info("received command", "id", cmd.ID, "type", cmd.Type)

	switch cmd.Type {
	case "provision", "deprovision":
		return m.handleRunCommand(ctx, cmd)
	case "reboot":
		slog.Info("reboot command received")
		if ackErr := m.deps.Client.AcknowledgeCommand(ctx, cmd.ID, "completed", ""); ackErr != nil {
			slog.Warn("failed to ACK command", "cmdID", cmd.ID, "error", ackErr)
		}
		return &commandResult{err: &RebootRequestedError{}}
	case "health-check":
		slog.Info("health-check command received")
		if ackErr := m.deps.Client.AcknowledgeCommand(ctx, cmd.ID, "completed", "healthy"); ackErr != nil {
			slog.Warn("failed to ACK command", "cmdID", cmd.ID, "error", ackErr)
		}
		return nil
	default:
		slog.Warn("unknown command type", "type", cmd.Type)
		if ackErr := m.deps.Client.AcknowledgeCommand(ctx, cmd.ID, "failed", "unknown command type"); ackErr != nil {
			slog.Warn("failed to ACK command", "cmdID", cmd.ID, "error", ackErr)
		}
		return nil
	}
}

// commandConfigProvider is the optional provider capability that merges a
// command payload over the base vars (caprf.Client.CommandConfig).
type commandConfigProvider interface {
	CommandConfig(payload []byte) (*config.MachineConfig, []string, error)
}

// handleRunCommand runs a provision or deprovision command with its own
// config: the base config, or the base vars with the command payload. The
// shared config of the client (m.deps.Cfg) is never written, because the
// log shipping and token renewal read it concurrently. A result of nil means
// that standby goes on.
func (m *StandbyMode) handleRunCommand(ctx context.Context, cmd config.Command) *commandResult {
	runCfg, keys, err := m.commandConfig(cmd.Payload)
	if err != nil {
		message := fmt.Sprintf("invalid %s payload: %v", cmd.Type, err)
		if errors.Is(err, errCopyConfig) {
			message = fmt.Sprintf("%s command failed: %v", cmd.Type, err)
		}
		slog.Error("command not run", "id", cmd.ID, "type", cmd.Type, "error", err)
		m.ack(ctx, cmd.ID, "failed", message)
		return nil
	}
	runCfg.Mode = cmd.Type
	if len(keys) > 0 {
		slog.Info("running command with payload", "id", cmd.ID, "type", cmd.Type,
			"keys", strings.Join(keys, ","), "hostname", runCfg.Hostname)
	}
	if cmd.Type == "provision" {
		return m.handleProvision(ctx, cmd, runCfg, keys)
	}
	return m.handleDeprovision(ctx, cmd, runCfg, keys)
}

// commandConfig returns a new config for one command. Without payload it is
// a copy of the base config. With a payload it is the base vars plus the
// payload, and the settings that the running ramdisk uses (transport,
// agent, network) keep their live values.
func (m *StandbyMode) commandConfig(payload []byte) (runCfg *config.MachineConfig, keys []string, err error) {
	live := m.deps.Cfg
	base, err := cloneConfig(live)
	if err != nil {
		return nil, nil, err
	}
	if len(payload) == 0 {
		return base, nil, nil
	}
	p, ok := m.deps.Client.(commandConfigProvider)
	if !ok {
		return nil, nil, fmt.Errorf("the provider does not support command payloads")
	}
	runCfg, keys, err = p.CommandConfig(payload)
	if err != nil {
		return nil, nil, err
	}
	if len(keys) == 0 { // only blank or comment lines
		return base, nil, nil
	}
	// The live settings, from the deep copy, so the run can not change them.
	runCfg.Transport = base.Transport
	runCfg.Agent = base.Agent
	runCfg.Network = base.Network
	runCfg.ProvisionerFiles = base.ProvisionerFiles
	runCfg.MachineFiles = base.MachineFiles
	runCfg.MachineCommands = base.MachineCommands
	return runCfg, keys, nil
}

// errCopyConfig marks a failed copy of the base config: not a payload error.
var errCopyConfig = errors.New("cannot copy the config")

// cloneConfig returns a deep copy of c, so that a run that changes slices,
// maps or pointers of its config does not change c.
func cloneConfig(c *config.MachineConfig) (*config.MachineConfig, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errCopyConfig, err)
	}
	var out config.MachineConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w: %w", errCopyConfig, err)
	}
	// Not in JSON.
	out.ProvisionerFiles = slices.Clone(c.ProvisionerFiles)
	out.MachineFiles = slices.Clone(c.MachineFiles)
	out.MachineCommands = slices.Clone(c.MachineCommands)
	return &out, nil
}

// ack reports a command result. It also works after ctx was canceled (the
// token stop cancels the run), with its own 10 second limit.
func (m *StandbyMode) ack(ctx context.Context, id, status, message string) {
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ackTimeout)
	defer cancel()
	if err := m.deps.Client.AcknowledgeCommand(ackCtx, id, status, message); err != nil {
		slog.Warn("failed to ACK command", "cmdID", id, "error", err)
	}
}

// ackMessage is the message of a completed command ACK.
func ackMessage(done string, keys []string) string {
	if len(keys) == 0 {
		return done
	}
	return done + " with payload keys " + strings.Join(keys, ",")
}

func (m *StandbyMode) handleProvision(ctx context.Context, cmd config.Command, cfg *config.MachineConfig, keys []string) *commandResult {
	orch := provision.NewOrchestrator(cfg, m.deps.Client, m.deps.DiskMgr)
	rescueCfg := orch.RescueConfig()
	var retryState rescue.RetryState
	var provErr error
	provisionSucceeded := false

	for {
		provErr = orch.Provision(ctx)
		if provErr == nil {
			provisionSucceeded = true
			break
		}
		var rebootErr *provision.RebootRequiredError
		if errors.As(provErr, &rebootErr) {
			// The firmware change needs a reset first; the command must run
			// again after the reboot into the ramdisk.
			m.ack(ctx, cmd.ID, "failed", "provision command needs a reboot first ("+rebootErr.Message()+
				"); send it again after the reboot")
			return &commandResult{err: &RebootRequestedError{}}
		}
		slog.Error("hot provision failed", "error", provErr)
		if setupErr := rescue.Setup(ctx, rescueCfg); setupErr != nil {
			slog.Warn("rescue setup error", "error", setupErr)
		}
		action := rescue.Decide(rescueCfg, &retryState)
		slog.Info("rescue action", "type", action.Type, "message", action.Message)

		switch action.Type {
		case rescue.ModeRetry:
			retryState.RecordAttempt(provErr)
			if !sleepWithContext(ctx, rescueCfg.RetryDelay) {
				return &commandResult{err: fmt.Errorf("retry canceled: %w", ctx.Err())}
			}
			continue
		case rescue.ModeShell:
			slog.Info("dropping to rescue shell")
			return &commandResult{err: &RescueShellError{}}
		case rescue.ModeWait:
			// ACK the failure first: standby reports every failed command.
			m.ack(ctx, cmd.ID, "failed", fmt.Sprintf("provision command failed: %v", provErr))
			slog.Info("waiting for manual intervention")
			<-ctx.Done()
			return &commandResult{err: fmt.Errorf("wait mode canceled: %w", ctx.Err())}
		default:
			// ModeReboot
		}
		break
	}

	if !provisionSucceeded {
		m.ack(ctx, cmd.ID, "failed", fmt.Sprintf("provision command failed: %v", provErr))
		return &commandResult{err: provErr}
	}

	m.ack(ctx, cmd.ID, "completed", ackMessage("provisioning complete", keys))

	done := provisionComplete(orch.FirmwareChanged(), cfg.PowerOffOnSuccess())
	done.Cfg = cfg // the handoff (kexec, power off) follows the command config
	return &commandResult{err: done}
}

func (m *StandbyMode) handleDeprovision(ctx context.Context, cmd config.Command, cfg *config.MachineConfig, keys []string) *commandResult {
	orch := m.deprovisioner(cfg)
	if err := orch.Deprovision(ctx); err != nil {
		slog.Error("hot deprovision failed", "error", err)
		m.ack(ctx, cmd.ID, "failed", fmt.Sprintf("deprovision command failed: %v", err))
		if cfg.WaitOnFailure() {
			slog.Info("waiting after deprovisioning failure")
			<-ctx.Done()
		}
		return &commandResult{err: err}
	}

	message := ackMessage("deprovisioning complete", keys)
	if s, ok := orch.(skippedDiskReporter); ok {
		message += provision.NotWipedNote(s.SkippedDisks())
	}
	if r, ok := orch.(eraseFallbackReporter); ok {
		message += provision.EraseFallbackNote(r.SecureEraseFallbacks())
	}
	m.ack(ctx, cmd.ID, "completed", message)
	if cfg.Agent.DeprovisionSuccessAction == config.AgentDeprovisionStandby {
		slog.Info("deprovisioning complete, returning to standby")
		if err := m.deps.Client.ReportStatus(ctx, config.StatusInit, "standby"); err != nil {
			slog.Warn("standby status after deprovisioning failed", "error", err)
		}
		return nil
	}
	if cfg.PowerOffOnSuccess() {
		return &commandResult{err: &PowerOffRequestedError{}}
	}
	return &commandResult{err: &RebootRequestedError{}}
}

func (m *StandbyMode) deprovisioner(cfg *config.MachineConfig) standbyDeprovisioner {
	if m.newDeprovisioner != nil {
		return m.newDeprovisioner(cfg)
	}
	return provision.NewOrchestrator(cfg, m.deps.Client, m.deps.DiskMgr)
}
