// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"log/slog"
	"os/exec"
	"sort"

	"github.com/johnnybravo-xyz/suchi/distro/internal/diagnostics"
)

func reportToolAvailability(log *slog.Logger) {
	tools := diagnostics.InspectTools(exec.LookPath)
	present := make([]string, 0, len(tools))
	missing := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Available {
			present = append(present, tool.ResolvedName)
		} else {
			missing = append(missing, tool.Name)
		}
	}
	sort.Strings(present)
	sort.Strings(missing)
	log.Info("main.tools.check", "present", present, "missing", missing)
	for _, tool := range tools {
		if tool.Available {
			continue
		}
		log.Warn("main.tools.missing",
			"tool", tool.Name,
			"impact", tool.Capability,
			"install", tool.Guidance)
	}
}
