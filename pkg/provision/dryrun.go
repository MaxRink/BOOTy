//go:build linux

package provision

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/health"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/pkg/network"
)

var (
	listInterfaces = net.Interfaces
	statPath       = os.Stat
	readPath       = os.ReadFile
)

// DryRunStatus represents the result status of a dry-run check.
type DryRunStatus string

// DryRunPass, DryRunWarn, and DryRunFail represent dry-run check outcomes.
const (
	DryRunPass DryRunStatus = "pass"
	DryRunWarn DryRunStatus = "warn"
	DryRunFail DryRunStatus = "fail"
)

// DryRunResult holds the result of a single dry-run check.
type DryRunResult struct {
	Step    string       `json:"step"`
	Status  DryRunStatus `json:"status"`
	Message string       `json:"message"`
}

// DryRun executes the provisioning pipeline in simulation mode.
func (o *Orchestrator) DryRun(ctx context.Context) error {
	o.log.Info("starting dry-run - no destructive changes will be made")

	checks := []struct {
		name string
		fn   func(ctx context.Context) DryRunResult
	}{
		{"config-validation", o.dryRunConfigValidation},
		{"image-reachability", o.dryRunImageReachability},
		{"image-prerequisites", o.dryRunImagePrerequisites},
		{"image-checksum", o.dryRunImageChecksum},
		{"image-signature", o.dryRunImageSignature},
		{"image-mode", o.dryRunImageMode},
		{"disk-detection", o.dryRunDiskDetection},
		{"network-link", o.dryRunNetworkLink},
		{"efi-boot", o.dryRunEFIBoot},
		{"health-checks", o.dryRunHealthChecks},
		{"inventory-probe", o.dryRunInventoryProbe},
	}

	results := make([]DryRunResult, 0, len(checks))
	var failed int
	var warned int
	for _, c := range checks {
		result := c.fn(ctx)
		result.Step = c.name // authoritative step name from the check table
		results = append(results, result)
		switch result.Status {
		case DryRunFail:
			failed++
		case DryRunWarn:
			warned++
		}
		o.log.Info("dry-run check", "step", result.Step, "status", string(result.Status), "message", result.Message)
	}

	var summary strings.Builder
	for _, r := range results {
		fmt.Fprintf(&summary, "[%s] %s: %s\n", r.Status, r.Step, r.Message)
	}

	if failed > 0 {
		msg := fmt.Sprintf("dry-run completed with %d failure(s):\n%s", failed, summary.String())
		if err := o.provider.ReportStatus(ctx, config.StatusError, msg); err != nil {
			o.log.Warn("failed to report dry-run status", "error", err)
		}
		return fmt.Errorf("dry-run: %d check(s) failed", failed)
	}

	if warned > 0 {
		msg := fmt.Sprintf("dry-run completed with %d warning(s):\n%s", warned, summary.String())
		if err := o.provider.ReportStatus(ctx, config.StatusSuccess, msg); err != nil {
			o.log.Warn("failed to report dry-run status", "error", err)
		}
		return nil
	}

	msg := fmt.Sprintf("dry-run passed all checks:\n%s", summary.String())
	if err := o.provider.ReportStatus(ctx, config.StatusSuccess, msg); err != nil {
		o.log.Warn("failed to report dry-run status", "error", err)
	}
	return nil
}

func (o *Orchestrator) dryRunConfigValidation(_ context.Context) DryRunResult {
	if err := o.validatePartitionLayoutConfig(); err != nil {
		return DryRunResult{Status: DryRunFail, Message: err.Error()}
	}

	if len(o.cfg.Provision.Image.URLs) == 0 {
		return DryRunResult{Status: DryRunFail, Message: "no image URLs configured"}
	}
	if err := config.ValidateRequiredProvisionTargetOS(o.cfg.Provision.TargetOS); err != nil {
		return DryRunResult{Status: DryRunFail, Message: fmt.Sprintf("rejected before destructive storage steps: %s", err)}
	}
	o.cfg.Provision.TargetOS = config.NormalizeProvisionTargetOS(o.cfg.Provision.TargetOS)
	osFamily := strings.ToLower(strings.TrimSpace(o.cfg.OSFamily))
	if osFamily == "rhel" {
		return DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("osFamily=%q is not supported for provisioning: rhel-like target bootloader support is not implemented: native GRUB2/BLS/vendor EFI paths are required before destructive storage steps", osFamily)}
	}
	if o.cfg.Hostname == "" {
		return DryRunResult{Status: DryRunWarn, Message: "hostname not set"}
	}
	return DryRunResult{Status: DryRunPass, Message: "configuration valid"}
}

func (o *Orchestrator) dryRunImageReachability(ctx context.Context) DryRunResult {
	if len(o.cfg.Provision.Image.URLs) == 0 {
		return DryRunResult{Status: DryRunFail, Message: "no image URLs configured"}
	}

	httpClient := network.NewHTTPClient(10 * time.Second)
	validated := 0
	for _, imgURL := range o.cfg.Provision.Image.URLs {
		scheme, redactedURL, invalidResult := validateDryRunImageURL(imgURL)
		if invalidResult != nil {
			return *invalidResult
		}

		if scheme == "oci" {
			if probeResult := probeOCIImageReachability(ctx, imgURL, redactedURL); probeResult != nil {
				return *probeResult
			}
		} else {
			probeResult := probeHTTPImageReachability(ctx, httpClient, imgURL, redactedURL)
			if probeResult != nil {
				return *probeResult
			}
		}

		validated++
		o.log.Info("image reachable", "url", redactedURL)
	}

	return DryRunResult{Status: DryRunPass, Message: fmt.Sprintf("all %d image URL(s) reachable", validated)}
}

func validateDryRunImageURL(imgURL string) (scheme, redactedURL string, invalidResult *DryRunResult) {
	redactedURL = redactImageURL(imgURL)
	parsedURL, err := url.Parse(imgURL)
	if err != nil || parsedURL.Scheme == "" {
		errMsg := redactURLError(err, imgURL)
		if errMsg == "" {
			errMsg = "missing URL scheme"
		}
		return "", redactedURL, &DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("invalid image URL %s: %s", redactedURL, errMsg)}
	}

	scheme = strings.ToLower(strings.TrimSpace(parsedURL.Scheme))
	if scheme != "http" && scheme != "https" && scheme != "oci" {
		return "", redactedURL, &DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("unsupported URL scheme %q for %s", scheme, redactedURL)}
	}

	return scheme, redactedURL, nil
}

func probeHTTPImageReachability(ctx context.Context, httpClient *http.Client, imgURL, redactedURL string) *DryRunResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, imgURL, http.NoBody)
	if err != nil {
		errMsg := redactURLError(err, imgURL)
		return &DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("invalid image URL %s: %s", redactedURL, errMsg)}
	}

	resp, err := httpClient.Do(req) //nolint:gosec // URL from trusted config
	if err != nil {
		errMsg := redactURLError(err, imgURL)
		return &DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("image unreachable %s: %s", redactedURL, errMsg)}
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close for validation probe

	if resp.StatusCode >= 400 {
		return &DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("image server returned %d for %s", resp.StatusCode, redactedURL)}
	}

	return nil
}

func probeOCIImageReachability(ctx context.Context, imgURL, redactedURL string) *DryRunResult {
	ref := trimDryRunOCIScheme(imgURL)
	probeCtx, cancel := context.WithTimeout(ctx, ociPreflightProbeTimeout)
	defer cancel()
	if err := probeOCIReference(probeCtx, ref); err != nil {
		errMsg := redactURLError(err, imgURL)
		return &DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("OCI image unreachable %s: %s", redactedURL, errMsg)}
	}
	return nil
}

func trimDryRunOCIScheme(imgURL string) string {
	const scheme = "oci://"
	if len(imgURL) >= len(scheme) && strings.EqualFold(imgURL[:len(scheme)], scheme) {
		return imgURL[len(scheme):]
	}
	return image.TrimOCIScheme(imgURL)
}

func (o *Orchestrator) dryRunDiskDetection(ctx context.Context) DryRunResult {
	if o.cfg.Provision.Disk.Device != "" {
		info, err := statPath(o.cfg.Provision.Disk.Device)
		if err != nil {
			return DryRunResult{Status: DryRunFail,
				Message: fmt.Sprintf("configured disk %s not found: %v", o.cfg.Provision.Disk.Device, err)}
		}
		// Reject character devices (e.g. /dev/null). This is intentionally
		// stricter than the real provisioning path to catch misconfigurations
		// early during dry-run validation.
		if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			return DryRunResult{Status: DryRunFail,
				Message: fmt.Sprintf("configured disk %s is not a block device", o.cfg.Provision.Disk.Device)}
		}
		return DryRunResult{Status: DryRunPass,
			Message: fmt.Sprintf("configured disk %s exists", o.cfg.Provision.Disk.Device)}
	}

	d, err := o.disk.DetectDisk(ctx, o.cfg.Provision.Disk.MinSizeGB)
	if err != nil {
		return DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("no suitable disk: %v", err)}
	}
	return DryRunResult{Status: DryRunPass,
		Message: fmt.Sprintf("detected disk %s", d)}
}

func (o *Orchestrator) dryRunImagePrerequisites(ctx context.Context) DryRunResult {
	if len(o.cfg.Provision.Image.URLs) == 0 {
		return DryRunResult{Status: DryRunFail, Message: "no image URLs configured"}
	}

	imageSources := dryRunImageSources(o.cfg.Provision.Image.URLs)
	if len(imageSources) == 0 {
		return DryRunResult{Status: DryRunFail, Message: "no image URLs configured"}
	}

	bestURL, err := image.SelectBestSource(ctx, imageSources)
	if err != nil {
		return DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("selecting image source: %v", err)}
	}
	if image.IsOCIReference(bestURL) {
		return DryRunResult{Status: DryRunWarn,
			Message: "OCI image selected: skipping local image prerequisite check"}
	}
	format, err := image.ValidateStreamingPrerequisites(ctx, bestURL)
	if err != nil {
		return DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("image prerequisites failed: %v", err)}
	}
	return DryRunResult{Status: DryRunPass,
		Message: fmt.Sprintf("image format %s prerequisites available", format)}
}

func dryRunImageSources(urls []string) []string {
	sources := make([]string, 0, len(urls))
	for _, source := range urls {
		source = strings.TrimSpace(source)
		if source != "" {
			sources = append(sources, source)
		}
	}
	return sources
}

func (o *Orchestrator) dryRunHealthChecks(ctx context.Context) DryRunResult {
	if !o.cfg.Health.Enabled {
		return DryRunResult{Status: DryRunWarn, Message: "health checks disabled"}
	}

	results, critical := health.RunAll(ctx, o.healthChecks(), o.cfg.Health.SkipChecks)
	o.logHealthCheckResults(results)
	if reporter, ok := o.provider.(HealthReporter); ok {
		if err := reporter.ReportHealthChecks(ctx, results); err != nil {
			o.log.Warn("failed to report health checks", "error", err)
		}
	}

	var failed []string
	var criticalFailed []string
	var skipped int
	for _, r := range results {
		switch r.Status {
		case health.StatusFail:
			failed = append(failed, r.Name)
			if r.Severity == health.SeverityCritical {
				criticalFailed = append(criticalFailed, r.Name)
			}
		case health.StatusSkip:
			skipped++
		}
	}

	summary := fmt.Sprintf(
		"health checks executed: %d pass, %d fail, %d skipped",
		len(results)-len(failed)-skipped,
		len(failed),
		skipped,
	)
	if critical {
		return DryRunResult{
			Status:  DryRunFail,
			Message: fmt.Sprintf("%s; critical failures: %s", summary, strings.Join(criticalFailed, ", ")),
		}
	}
	if len(failed) > 0 {
		return DryRunResult{
			Status:  DryRunWarn,
			Message: fmt.Sprintf("%s; non-critical failures: %s", summary, strings.Join(failed, ", ")),
		}
	}
	return DryRunResult{Status: DryRunPass, Message: summary}
}

func (o *Orchestrator) dryRunImageChecksum(_ context.Context) DryRunResult {
	if o.cfg.Provision.Image.Checksum == "" {
		return DryRunResult{Status: DryRunWarn,
			Message: "no image checksum configured - integrity cannot be verified"}
	}
	checksum, err := image.NormalizeChecksum(
		o.cfg.Provision.Image.Checksum,
		o.cfg.Provision.Image.ChecksumType,
	)
	if err != nil {
		return DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("invalid image checksum: %v", err)}
	}
	return DryRunResult{Status: DryRunPass,
		Message: fmt.Sprintf("checksum configured (%s)", checksum.ChecksumType)}
}

func (o *Orchestrator) dryRunImageSignature(_ context.Context) DryRunResult {
	o.cfg.Provision.Image.SignatureURL = strings.TrimSpace(o.cfg.Provision.Image.SignatureURL)
	if o.cfg.Provision.Image.SignatureURL == "" {
		return DryRunResult{Status: DryRunWarn,
			Message: "no image signature URL configured - GPG verification disabled"}
	}
	if strings.TrimSpace(o.cfg.Provision.Image.Checksum) == "" {
		return DryRunResult{Status: DryRunFail,
			Message: imageSignatureChecksumRequiredMessage}
	}
	if o.cfg.Provision.Image.GPGPubKey == "" {
		return DryRunResult{Status: DryRunFail,
			Message: "image signature URL set but no GPG public key path configured"}
	}
	if _, err := statPath(o.cfg.Provision.Image.GPGPubKey); err != nil {
		return DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("GPG public key not found: %s", o.cfg.Provision.Image.GPGPubKey)}
	}
	return DryRunResult{Status: DryRunPass,
		Message: "image signature verification configured"}
}

func (o *Orchestrator) dryRunImageMode(_ context.Context) DryRunResult {
	mode := strings.ToLower(strings.TrimSpace(o.cfg.Provision.Image.Mode))
	if mode == "" || mode == "whole-disk" {
		return DryRunResult{Status: DryRunPass, Message: "image mode: whole-disk (default)"}
	}
	if mode == "partition" {
		return DryRunResult{Status: DryRunPass,
			Message: "image mode: partition-by-partition (requires ramdisk + losetup)"}
	}
	if mode == "ab" {
		return DryRunResult{Status: DryRunPass,
			Message: "image mode: A/B dual-root slot update"}
	}
	return DryRunResult{Status: DryRunFail,
		Message: fmt.Sprintf("unknown IMAGE_MODE: %q (valid: whole-disk, partition, ab)", o.cfg.Provision.Image.Mode)}
}

func (o *Orchestrator) dryRunNetworkLink(_ context.Context) DryRunResult {
	ifaces, err := listInterfaces()
	if err != nil {
		return DryRunResult{Status: DryRunFail,
			Message: fmt.Sprintf("cannot list interfaces: %v", err)}
	}

	var upIfaces []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Skip virtual interfaces (veth, docker, bridges).
		if isVirtualInterface(iface.Name) {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if interfaceHasCarrier(iface.Name) {
			upIfaces = append(upIfaces, iface.Name)
		}
	}

	if len(upIfaces) == 0 {
		return DryRunResult{Status: DryRunFail,
			Message: "no physical non-loopback interfaces are up"}
	}
	return DryRunResult{Status: DryRunPass,
		Message: fmt.Sprintf("interfaces up: %s", strings.Join(upIfaces, ", "))}
}

func interfaceHasCarrier(name string) bool {
	carrierPath := fmt.Sprintf("/sys/class/net/%s/carrier", name)
	if carrierRaw, err := readPath(carrierPath); err == nil {
		return strings.TrimSpace(string(carrierRaw)) == "1"
	}

	operStatePath := fmt.Sprintf("/sys/class/net/%s/operstate", name)
	operStateRaw, err := readPath(operStatePath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(operStateRaw)) == "up"
}

// isVirtualInterface returns true for known virtual interface name prefixes.
func isVirtualInterface(name string) bool {
	virtualPrefixes := []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "cali", "tunl", "vxlan"}
	for _, prefix := range virtualPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (o *Orchestrator) dryRunEFIBoot(_ context.Context) DryRunResult {
	// Check EFI variables directory exists
	if _, err := statPath("/sys/firmware/efi"); err != nil {
		return DryRunResult{Status: DryRunWarn,
			Message: "system not booted in EFI mode"}
	}
	return DryRunResult{Status: DryRunPass,
		Message: "EFI firmware detected"}
}

func redactImageURL(rawURL string) string {
	return image.RedactURL(rawURL)
}

func redactURLError(err error, rawURL string) string {
	return image.RedactSourceError(err, rawURL)
}

func (o *Orchestrator) dryRunInventoryProbe(_ context.Context) DryRunResult {
	if !o.cfg.Provision.Inventory.Enabled {
		return DryRunResult{Status: DryRunWarn,
			Message: "hardware inventory disabled"}
	}
	// Check DMI data accessible
	if _, err := statPath("/sys/class/dmi/id/sys_vendor"); err != nil {
		return DryRunResult{Status: DryRunWarn,
			Message: "DMI data not accessible"}
	}
	return DryRunResult{Status: DryRunPass,
		Message: "hardware inventory enabled, DMI accessible"}
}
