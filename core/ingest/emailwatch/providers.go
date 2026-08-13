package emailwatch

import "sort"

// Provider presets exist so the UI can drop users into a working
// IMAP config with one click for the ~six mail hosts that cover most
// of the install base. Each entry captures the host/port/TLS choice
// plus the auth flavor (device-code vs app-password) so the settings
// form can render the correct next-step affordance. "custom" is the
// escape hatch — an empty host means "let the operator fill it in".

// Preset is one provider's canonical IMAP settings + UI hints.
type Preset struct {
	Host         string
	Port         int
	UseTLS       bool
	AuthMethod   string // password | xoauth2
	HelpText     string // rendered under the preset in UI
	LearnMoreURL string
}

// Presets is the provider table. Keys are stable identifiers used by
// the API + settings form; values are the display + wiring data.
var Presets = map[string]Preset{
	"microsoft": {
		Host: "outlook.office365.com", Port: 993, UseTLS: true, AuthMethod: "xoauth2",
		HelpText: "Outlook / M365 — click Sign in with Microsoft to complete the device-code flow.",
	},
	"gmail": {
		Host: "imap.gmail.com", Port: 993, UseTLS: true, AuthMethod: "password",
		HelpText: "Gmail — requires a Google App Password (2FA on).",
	},
	"fastmail": {
		Host: "imap.fastmail.com", Port: 993, UseTLS: true, AuthMethod: "password",
		HelpText: "Fastmail — generate an app password under Settings → Password & Security.",
	},
	"icloud": {
		Host: "imap.mail.me.com", Port: 993, UseTLS: true, AuthMethod: "password",
		HelpText: "iCloud — requires an app-specific password from appleid.apple.com.",
	},
	"proton": {
		Host: "protonmail-bridge", Port: 143, UseTLS: false, AuthMethod: "password",
		HelpText: "Proton Bridge via the socat relay (host = protonmail-bridge, port 143).",
	},
	"zoho": {
		Host: "imap.zoho.com", Port: 993, UseTLS: true, AuthMethod: "password",
		HelpText: "Zoho — generate an app password under Security → App Passwords.",
	},
	"custom": {
		AuthMethod: "password",
		HelpText:   "Configure host/port/TLS by hand.",
	},
}

// PresetByName returns the named preset + ok. Callers pass ok=false
// through to the UI as "unknown provider" rather than falling back
// silently. (Named PresetByName rather than Preset because the type
// already owns that identifier in this package.)
func PresetByName(name string) (Preset, bool) {
	p, ok := Presets[name]
	return p, ok
}

// PresetNames returns the preset keys in stable (alphabetical) order
// so dropdown rendering is deterministic across page loads.
func PresetNames() []string {
	names := make([]string, 0, len(Presets))
	for k := range Presets {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
