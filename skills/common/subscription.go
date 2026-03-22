package common

import (
	"strings"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

const enterprisePlusLicense = "Enterprise Plus"

// IsEvidenceSupported checks whether the server's license supports evidence upload.
// It calls GET /api/system/license to retrieve the license type.
//   - If the check succeeds and the license is not Enterprise+, evidence is not supported.
//   - If the check fails for any reason (403, network error, cloud instance, etc.),
//     we fall back to attempting evidence upload and let the server respond.
func IsEvidenceSupported(serverDetails *config.ServerDetails) bool {
	sm, err := utils.CreateServiceManager(serverDetails, 3, 0, false)
	if err != nil {
		log.Debug("License check skipped: could not create service manager:", err.Error())
		return true
	}
	info, err := sm.GetLicense()
	if err != nil {
		log.Debug("License check skipped: could not fetch server license:", err.Error())
		return true
	}
	if info == nil {
		return true
	}
	return strings.Contains(info.Type, enterprisePlusLicense)
}
