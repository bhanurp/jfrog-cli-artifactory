# Xray Malicious Skill Auto-Deletion on Publish

## Problem

When a skill is published via `jf skills publish`, there is no mechanism to detect and remove malicious content. Xray auto-indexes the artifact in the background, but the CLI does not check the results. A malicious skill can sit in the repository until the periodic cron cleanup catches it, leaving a window of exposure.

## Goal

Integrate Xray scan result checking into the `jf skills publish` flow. After upload, poll Xray until the scan completes, check for malicious findings via the Artifact Summary API, and automatically delete the skill if it's flagged. This provides near-real-time remediation.

## Scope

- Individual skill packages uploaded via `jf skills publish`
- Package type: Skills (zip archives in local/federated repos)
- Only the publish flow is modified; install and search are unchanged

## Non-Goals

- Configuring Xray watches or policies (relies on auto-indexing)
- Webhook-based event-driven detection
- Scanning skills at install time

## Architecture

### Approach: Composable Function

A standalone `VerifyAndRemediate` function in `skills/common/` is called from the publish command after upload + evidence. This function is reusable by the existing periodic cron cleanup and any future `jf skills verify` command.

### API Flow (all APIs already exist in jfrog-client-go)

| Step | API Endpoint | XrayServicesManager Method |
|------|-------------|---------------------------|
| 1. Poll scan status | `POST /api/v1/artifact/status` | `GetArtifactStatus(repo, path)` |
| 2. Get vulnerabilities | `POST /api/v2/summary/artifact` | `ArtifactSummary(params)` |
| 3. Delete if malicious | `DELETE /{repo}/{path}` | Artifactory service manager |

### Sequence

```
publish.go: upload() completes
    |
    +-- attachEvidence() (existing, unchanged)
    |
    +-- verifyAndRemediate(serverDetails, repoKey, slug, version)
         |  (skipped if --skip-scan or JFROG_CLI_SKIP_SKILLS_SCAN=true)
         |
         +-- 1. Poll artifact scan status
         |      xrayManager.GetArtifactStatus(repoKey, "{slug}/{version}/{slug}-{version}.zip")
         |      Wait until Overall.Status == DONE or PARTIAL (5 min timeout, 5 sec interval)
         |      - PENDING/SCANNING: keep polling
         |      - DONE/PARTIAL: proceed to step 2 (PARTIAL means some results available)
         |      - Timeout: warn "Scan did not complete in time" -> return nil (non-fatal)
         |      - FAILED/NOT_SUPPORTED/NOT_SCANNED: warn -> return nil (non-fatal)
         |
         +-- 2. Get artifact summary
         |      xrayManager.ArtifactSummary(ArtifactSummaryParams{
         |        Paths: ["{repoKey}/{slug}/{version}/{slug}-{version}.zip"]
         |      })
         |      If response.Artifacts is empty: warn "No scan results available" -> return nil
         |      Check response.Artifacts[0].Issues[] for malicious indicators (see Detection Criteria)
         |
         +-- 3a. If malicious issues found:
         |      - Log issue details (IssueId, Summary, Severity, CVEs)
         |      - Delete via Artifactory service manager: {repoKey}/{slug}/{version}/ (entire version dir)
         |      - Log "Skill '{slug}' v{version} deleted: malicious content detected"
         |      - Return error (causes non-zero exit code)
         |
         +-- 3b. If clean:
              - Log "Scan complete. No malicious content detected."
              - Return nil
```

## Repository Changes

### jfrog-client-go: No changes needed

All required APIs already exist:
- `xray/manager.go:267` - `GetArtifactStatus(repo, path)` -> calls `ArtifactService.GetStatus()`
- `xray/manager.go:245` - `ArtifactSummary(params)` -> calls `SummaryService.GetArtifactSummary()`
- `artifactory/services/delete.go` - `DeleteService` for artifact deletion

### jfrog-cli-artifactory: All changes here

#### New file: `skills/common/xray_scan.go`

Contains the composable `VerifyAndRemediate` function:

```go
const (
    scanPollTimeout  = 5 * time.Minute
    scanPollInterval = 5 * time.Second
)

func VerifyAndRemediate(serverDetails *config.ServerDetails, repoKey, slug, version string) error
```

Responsibilities:
1. Create `XrayServicesManager` from server details (using `xrayutils.CreateXrayServiceManager` from jfrog-cli-core)
2. Poll `xrayManager.GetArtifactStatus()` with timeout
3. Call `xrayManager.ArtifactSummary()` on scan completion (DONE or PARTIAL)
4. Evaluate issues for malicious content via `isMalicious()`
5. Delete artifact via Artifactory service manager if malicious (delete entire version directory)
6. Return error if deleted, nil if clean or scan inconclusive

Helper functions:
- `pollScanStatus(xrayManager, repo, path, timeout, interval) (ArtifactStatus, error)` - polling loop
- `isMalicious(issues []services.Issue) bool` - evaluates issues for malicious indicators
- `deleteSkillArtifact(serverDetails, repoKey, slug, version) error` - deletes `{repoKey}/{slug}/{version}/` using standard Artifactory service manager (same pattern as existing upload code)

#### Modified file: `skills/commands/publish/publish.go`

Add `skipScan` field to `PublishCommand` struct with `SetSkipScan(bool)` setter (consistent with existing `SetQuiet()` pattern).

After `attachEvidence()` call (around line 152), add:

```go
if !pc.skipScan {
    if err := common.VerifyAndRemediate(pc.serverDetails, pc.repoKey, slug, version); err != nil {
        return err
    }
}
```

Wire `skipScan` from flag and env var in `RunPublish()`:

```go
skipScan := c.GetBoolFlagValue("skip-scan") || os.Getenv("JFROG_CLI_SKIP_SKILLS_SCAN") == "true"
```

#### Modified file: `skills/cli/cli.go`

Add `--skip-scan` flag to the publish command flags list.

#### Modified file: `cliutils/flagkit/flags.go`

Define the `skip-scan` flag:
```
SkipScan = "skip-scan"
```
Usage: `"[Default: false] Skip Xray malicious content scan after publishing. Can also be set via JFROG_CLI_SKIP_SKILLS_SCAN=true."`

#### New file: `skills/common/xray_scan_test.go`

Unit tests:
- `TestPollScanStatus_Done` - returns immediately when status is DONE
- `TestPollScanStatus_Partial` - returns when status is PARTIAL
- `TestPollScanStatus_Timeout` - returns nil error after timeout
- `TestPollScanStatus_PendingThenDone` - polls multiple times then succeeds
- `TestPollScanStatus_Failed` - returns nil error with FAILED status
- `TestIsMalicious_MaliciousPackage` - detects malicious (JFrog provider, Critical, malicious in summary)
- `TestIsMalicious_CriticalCVE_NotMalicious` - Critical CVE without malicious indicator returns false
- `TestIsMalicious_LowSeverity` - returns false
- `TestIsMalicious_NoIssues` - returns false
- `TestVerifyAndRemediate_Clean` - full flow, no deletion
- `TestVerifyAndRemediate_Malicious` - full flow, deletion triggered, asserts correct deletion path
- `TestVerifyAndRemediate_EmptyArtifacts` - empty response, warns, no deletion
- `TestVerifyAndRemediate_SkipScan` - env var bypasses check

## Malicious Detection Criteria

The `Issue` struct from Artifact Summary (`xray/services/summary.go`) contains:
- `IssueType` (string): "security", "license", "operational_risk"
- `Severity` (string): "Critical", "High", "Medium", "Low", "Information"
- `Summary` (string): human-readable description
- `Provider` (string): source of the finding (e.g., "JFrog")

Detection logic in `isMalicious()`:
1. Filter issues where `IssueType == "security"`
2. Check if any security issue matches ALL of:
   - `Severity == "Critical"`
   - `Provider == "JFrog"` (JFrog's malicious package database, not third-party CVE feeds)
   - `Summary` contains "malicious" (case-insensitive)
3. Return true if any match found

**Rationale:** Not all Critical security issues are malicious. A skill could have a Critical CVE in a dependency (e.g., Log4j) without being a malicious package. We narrow detection to JFrog's own malicious package findings by requiring the JFrog provider AND malicious keyword in the summary. This prevents false positive deletions of legitimate skills with critical vulnerabilities while still catching actual malicious packages.

## CLI UX

### Happy path (clean skill):
```
$ jf skills publish ./my-skill --repo skills-local
Publishing skill 'my-skill' version '1.0.0'...
Skill 'my-skill' version '1.0.0' published successfully.
Scanning for malicious content...
Scan complete. No malicious content detected.
```

### Malicious skill detected:
```
$ jf skills publish ./bad-skill --repo skills-local
Publishing skill 'bad-skill' version '1.0.0'...
Skill 'bad-skill' version '1.0.0' published successfully.
Scanning for malicious content...
[ERROR] Malicious content detected in skill 'bad-skill' v1.0.0:
  - XRAY-12345: Malicious package detected (Critical)
    CVEs: CVE-2026-1234
Deleting skill 'bad-skill' v1.0.0 from repository...
Skill 'bad-skill' v1.0.0 deleted due to malicious content detection.
$ echo $?
1
```

### Scan timeout:
```
$ jf skills publish ./my-skill --repo skills-local
Publishing skill 'my-skill' version '1.0.0'...
Skill 'my-skill' version '1.0.0' published successfully.
Scanning for malicious content...
[WARN] Xray scan did not complete within 5 minutes. Skill published without scan verification.
```

### Partial scan:
```
$ jf skills publish ./my-skill --repo skills-local
Publishing skill 'my-skill' version '1.0.0'...
Skill 'my-skill' version '1.0.0' published successfully.
Scanning for malicious content...
Scan partially complete. No malicious content detected in available results.
```

### Skip scan:
```
$ jf skills publish ./my-skill --repo skills-local --skip-scan
Publishing skill 'my-skill' version '1.0.0'...
Skill 'my-skill' version '1.0.0' published successfully.
Xray scan check skipped (--skip-scan).
```

## Configuration

| Mechanism | Purpose |
|-----------|---------|
| `--skip-scan` flag | Skip post-publish Xray scan check |
| `JFROG_CLI_SKIP_SKILLS_SCAN` env var | Same as --skip-scan, for CI pipelines |
| Polling timeout: 5 minutes | Package-level constant `scanPollTimeout` |
| Polling interval: 5 seconds | Package-level constant `scanPollInterval` |

**Note on env vars:** The existing `JFROG_CLI_DISABLE_SKILLS_SCAN` controls the pre-publish Xray indexing _warning_ (`WarnIfXrayDisabled`). The new `JFROG_CLI_SKIP_SKILLS_SCAN` controls the post-publish scan _verification and remediation_. These are separate concerns: a user may want the warning (to know Xray isn't indexing) but skip the blocking poll, or vice versa. The `--skip-scan` flag documentation explicitly mentions the env var for discoverability.

## Error Handling

| Scenario | Behavior | Exit Code |
|----------|----------|-----------|
| Scan completes (DONE), no issues | Log success | 0 |
| Scan partial (PARTIAL), no issues | Log partial success | 0 |
| Scan completes, malicious found | Delete version dir + error message | 1 |
| Scan timeout (5 min) | Warn, skill stays | 0 |
| Xray not available / API error | Warn, skill stays | 0 |
| Scan status FAILED | Warn, skill stays | 0 |
| Scan status NOT_SUPPORTED | Warn, skill stays | 0 |
| Scan status NOT_SCANNED | Warn, skill stays | 0 |
| Empty Artifacts in summary response | Warn "No scan results available", skill stays | 0 |
| Deletion fails after malicious detection | Error with both scan finding and deletion failure | 1 |

Design principle: Only block on confirmed malicious findings. All inconclusive states (timeout, errors, empty results, failures) are non-fatal warnings. The periodic cron acts as the safety net.

## Testing Strategy

### Unit tests (skills/common/xray_scan_test.go)
- Mock HTTP responses at `http.RoundTripper` level for Xray APIs
- Table-driven tests for `isMalicious()` with various issue combinations
- Polling behavior: DONE, PARTIAL, timeout, FAILED, NOT_SUPPORTED
- Empty artifacts response handling
- Skip-scan env var behavior
- Deletion path assertion: verify `{repoKey}/{slug}/{version}/` is the exact path passed to delete

### E2E tests
- Publish a clean skill to a real Artifactory with Xray enabled -> verify no deletion
- Publish with --skip-scan -> verify scan is skipped
- Scan timeout scenario (if testable with short timeout override)

## Assumptions

1. Xray is configured to auto-index the skills repository (XrayIndex=true)
2. The Artifact Summary API (`POST /api/v2/summary/artifact`) returns issues for auto-indexed artifacts without requiring a watch
3. JFrog's malicious package detection surfaces as issues with `Provider: "JFrog"`, `IssueType: "security"`, `Severity: "Critical"`, and `Summary` containing "malicious"
4. The artifact path format is `{slug}/{version}/{slug}-{version}.zip` within the repo (confirmed from `publish.go` upload target and `zipSkillFolder()`)
5. The existing `JFROG_CLI_DISABLE_SKILLS_SCAN` (pre-publish warning) and new `JFROG_CLI_SKIP_SKILLS_SCAN` (post-publish verification) are separate controls for separate concerns
6. Deletion uses the same Artifactory server details and auth as the upload
7. Deleting the entire version directory `{repoKey}/{slug}/{version}/` is the correct cleanup scope (removes zip and any associated metadata)
8. `PARTIAL` scan status means some results are available and should be checked (not treated as still-in-progress)
