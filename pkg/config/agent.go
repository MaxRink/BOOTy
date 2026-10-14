package config

// AgentConfig holds settings for the standby/agent operating mode.
// In standby mode, BOOTy keeps the machine warm in the ramdisk, sending
// heartbeats and polling for commands. When a command arrives (provision,
// deprovision, reboot), it executes immediately without a full PXE cycle.
type AgentConfig struct {
	// HeartbeatURL is the POST endpoint for periodic keepalive signals.
	// Default: ""
	HeartbeatURL string `yaml:"heartbeatURL" json:"heartbeatURL"`

	// CommandsURL is the GET endpoint for polling pending commands.
	// Default: ""
	CommandsURL string `yaml:"commandsURL" json:"commandsURL"`

	// DeprovisionSuccessAction is what standby mode does after a successful
	// deprovision command.
	// "standby": acknowledge the command and go back to the standby loop in
	// the same boot, without a reboot or power off.
	// Valid values: "standby". Not preset by legacyCompat.
	// Default: "" (end the run like SuccessAction)
	DeprovisionSuccessAction string `yaml:"deprovisionSuccessAction" json:"deprovisionSuccessAction"`
}

// AgentDeprovisionStandby is the AgentConfig.DeprovisionSuccessAction value
// that returns to the standby loop.
const AgentDeprovisionStandby = "standby"
