// Package presetfile validates raw HuML and TOML taxonomy files before
// normalizing them into the application's filing tree.
package presetfile

const Format = "suchi-taxonomy/v1"

// PresetFile is the runtime tree returned by Parse. Inbox is generated, never authored.
type PresetFile struct {
	Format     string     `huml:"format" toml:"format" json:"format"`
	System     string     `huml:"system,omitempty" toml:"system,omitempty" json:"system,omitempty"`
	ID         string     `huml:"id" toml:"id" json:"id"`
	Version    int        `huml:"version" toml:"version" json:"version"`
	Name       string     `huml:"name" toml:"name" json:"name"`
	Market     string     `huml:"market" toml:"market" json:"market"`
	Language   string     `huml:"language" toml:"language" json:"language"`
	Maintainer string     `huml:"maintainer,omitempty" toml:"maintainer,omitempty" json:"maintainer,omitempty"`
	License    string     `huml:"license,omitempty" toml:"license,omitempty" json:"license,omitempty"`
	Flat       bool       `huml:"flat,omitempty" toml:"flat,omitempty" json:"flat,omitempty"`
	Inbox      int        `huml:"-" toml:"-" json:"-"`
	Story      string     `huml:"story" toml:"story" json:"story"`
	Areas      []Area     `huml:"areas" toml:"areas" json:"areas"`
	Categories []Category `huml:"categories,omitempty" toml:"categories,omitempty" json:"categories,omitempty"`
	Seeds      *Seeds     `huml:"seeds,omitempty" toml:"seeds,omitempty" json:"seeds,omitempty"`
}

type Area struct {
	Code       int        `huml:"code" toml:"code" json:"code"`
	Name       string     `huml:"name" toml:"name" json:"name"`
	Categories []Category `huml:"categories" toml:"categories" json:"categories"`
}

type Category struct {
	Code        int      `huml:"code" toml:"code" json:"code"`
	Name        string   `huml:"name" toml:"name" json:"name"`
	Description string   `huml:"description,omitempty" toml:"description,omitempty" json:"description,omitempty"`
	Keywords    []string `huml:"keywords,omitempty" toml:"keywords,omitempty" json:"keywords,omitempty"`
}

type Seeds struct {
	Automations []SeedAutomation `huml:"automations,omitempty" toml:"automations,omitempty" json:"automations,omitempty"`
}

type SeedAutomation struct {
	Name    string   `huml:"name" toml:"name" json:"name"`
	Trigger Trigger  `huml:"trigger" toml:"trigger" json:"trigger"`
	Actions []Action `huml:"actions" toml:"actions" json:"actions"`
}

// Trigger uses runtime trigger codes and portable symbolic filters, never row IDs.
type Trigger struct {
	Type                     int    `huml:"type" toml:"type" json:"type"`
	FilterPath               string `huml:"filter_path,omitempty" toml:"filter_path,omitempty" json:"filter_path,omitempty"`
	FilterFilename           string `huml:"filter_filename,omitempty" toml:"filter_filename,omitempty" json:"filter_filename,omitempty"`
	FilterTitleMatching      string `huml:"filter_title_matching,omitempty" toml:"filter_title_matching,omitempty" json:"filter_title_matching,omitempty"`
	FilterContentMatching    string `huml:"filter_content_matching,omitempty" toml:"filter_content_matching,omitempty" json:"filter_content_matching,omitempty"`
	FilterTag                string `huml:"filter_has_tag,omitempty" toml:"filter_has_tag,omitempty" json:"filter_has_tag,omitempty"`
	FilterCorrespondent      string `huml:"filter_has_correspondent,omitempty" toml:"filter_has_correspondent,omitempty" json:"filter_has_correspondent,omitempty"`
	FilterDocumentType       string `huml:"filter_has_document_type,omitempty" toml:"filter_has_document_type,omitempty" json:"filter_has_document_type,omitempty"`
	FilterEmailFrom          string `huml:"filter_email_from,omitempty" toml:"filter_email_from,omitempty" json:"filter_email_from,omitempty"`
	FilterEmailSubject       string `huml:"filter_email_subject,omitempty" toml:"filter_email_subject,omitempty" json:"filter_email_subject,omitempty"`
	FilterEmailFolder        string `huml:"filter_email_folder,omitempty" toml:"filter_email_folder,omitempty" json:"filter_email_folder,omitempty"`
	FilterEmailHasAttachment *bool  `huml:"filter_email_has_attachment,omitempty" toml:"filter_email_has_attachment,omitempty" json:"filter_email_has_attachment,omitempty"`
}

// Action parameters are validated against the exact portable subset, before import.
type Action struct {
	Kind   string         `huml:"kind" toml:"kind" json:"kind"`
	Params map[string]any `huml:"params" toml:"params" json:"params"`
}
