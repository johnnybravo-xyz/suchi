// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mimeutil owns Suchi's document MIME registry. Format routing,
// filename refinement, and output extensions must derive from this table.
package mimeutil

import "strings"

const (
	ApplicationOctetStream  = "application/octet-stream"
	ApplicationOLEStorage   = "application/x-ole-storage"
	ApplicationZip          = "application/zip"
	ApplicationPDF          = "application/pdf"
	ApplicationEPUBZip      = "application/epub+zip"
	ApplicationMSWord       = "application/msword"
	ApplicationRTF          = "application/rtf"
	ApplicationMSExcel      = "application/vnd.ms-excel"
	ApplicationMSOutlook    = "application/vnd.ms-outlook"
	ApplicationMSPowerPoint = "application/vnd.ms-powerpoint"
	ApplicationODP          = "application/vnd.oasis.opendocument.presentation"
	ApplicationODS          = "application/vnd.oasis.opendocument.spreadsheet"
	ApplicationODT          = "application/vnd.oasis.opendocument.text"
	ApplicationPPTX         = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	ApplicationXLSX         = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	ApplicationDOCX         = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	ImageGIF                = "image/gif"
	ImageHEIC               = "image/heic"
	ImageHEIF               = "image/heif"
	ImageJPEG               = "image/jpeg"
	ImagePNG                = "image/png"
	ImageTIFF               = "image/tiff"
	ImageDjVu               = "image/vnd.djvu"
	ImageWebP               = "image/webp"
	MessageRFC822           = "message/rfc822"
	TextCSV                 = "text/csv"
	TextHTML                = "text/html"
	TextMarkdown            = "text/markdown"
	TextPlain               = "text/plain"
)

type formatFlags uint8

const (
	formatPDF formatFlags = 1 << iota
	formatAnyDoc
	formatRasterImage
	formatEmail
	formatOutlook
	formatDjVu
	formatDirectText
)

// Format is the normalized document-format contract for a MIME label.
// Unknown labels retain their normalized MIME and have no extension or
// format-specific capabilities.
type Format struct {
	MIME      string
	Extension string
	flags     formatFlags
}

func (f Format) IsPDF() bool         { return f.flags&formatPDF != 0 }
func (f Format) IsAnyDoc() bool      { return f.flags&formatAnyDoc != 0 }
func (f Format) IsRasterImage() bool { return f.flags&formatRasterImage != 0 }
func (f Format) IsEmail() bool       { return f.flags&formatEmail != 0 }
func (f Format) IsOutlook() bool     { return f.flags&formatOutlook != 0 }
func (f Format) IsDjVu() bool        { return f.flags&formatDjVu != 0 }
func (f Format) IsDirectText() bool  { return f.flags&formatDirectText != 0 }
func (f Format) IsImage() bool       { return strings.HasPrefix(f.MIME, "image/") }

type formatDefinition struct {
	mime               string
	extension          string
	filenameExtensions []string
	flags              formatFlags
}

var formatDefinitions = []formatDefinition{
	// PDF.
	{ApplicationPDF, ".pdf", nil, formatPDF},

	// EPUB and office documents converted by anydoc.
	{ApplicationEPUBZip, ".epub", []string{".epub"}, formatAnyDoc},
	{"application/epub", ".epub", nil, formatAnyDoc},
	{ApplicationMSWord, ".doc", []string{".doc"}, formatAnyDoc},
	{ApplicationDOCX, ".docx", []string{".docx"}, formatAnyDoc},
	{"application/vnd.ms-word.document.macroenabled.12", ".docm", []string{".docm"}, formatAnyDoc},
	{ApplicationMSPowerPoint, ".ppt", []string{".pot", ".pps", ".ppt"}, formatAnyDoc},
	{ApplicationPPTX, ".pptx", []string{".pptx"}, formatAnyDoc},
	{"application/vnd.ms-powerpoint.presentation.macroenabled.12", ".pptm", []string{".pptm"}, formatAnyDoc},
	{"application/vnd.openxmlformats-officedocument.presentationml.slideshow", ".ppsx", []string{".ppsx"}, formatAnyDoc},
	{"application/vnd.ms-powerpoint.slideshow.macroenabled.12", ".ppsm", []string{".ppsm"}, formatAnyDoc},
	{ApplicationMSExcel, ".xls", []string{".xls"}, formatAnyDoc},
	{ApplicationXLSX, ".xlsx", []string{".xlsx"}, formatAnyDoc},
	{"application/vnd.ms-excel.sheet.macroenabled.12", ".xlsm", []string{".xlsm"}, formatAnyDoc},
	{"application/vnd.ms-excel.sheet.binary.macroenabled.12", ".xlsb", []string{".xlsb"}, formatAnyDoc},
	{ApplicationODT, ".odt", []string{".odt"}, formatAnyDoc},
	{ApplicationODS, ".ods", []string{".ods"}, formatAnyDoc},
	{ApplicationODP, ".odp", []string{".odp"}, formatAnyDoc},
	{ApplicationRTF, ".rtf", []string{".rtf"}, formatAnyDoc},
	{"text/rtf", ".rtf", nil, formatAnyDoc},
	{TextCSV, ".csv", []string{".csv"}, formatAnyDoc},

	// Raster images converted to searchable PDFs.
	{ImageJPEG, ".jpg", nil, formatRasterImage},
	{"image/jpg", ".jpg", nil, formatRasterImage},
	{"image/pjpeg", ".jpg", nil, formatRasterImage},
	{ImagePNG, ".png", nil, formatRasterImage},
	{ImageTIFF, ".tiff", nil, formatRasterImage},
	{"image/x-tiff", ".tiff", nil, formatRasterImage},
	{ImageHEIC, ".heic", []string{".heic"}, formatRasterImage},
	{ImageHEIF, ".heif", []string{".heif"}, formatRasterImage},
	{"image/heic-sequence", ".heic", nil, formatRasterImage},
	{"image/heif-sequence", ".heif", nil, formatRasterImage},
	{ImageWebP, ".webp", nil, formatRasterImage},
	{ImageGIF, ".gif", nil, formatRasterImage},
	{"image/bmp", ".bmp", nil, formatRasterImage},
	{"image/x-bmp", ".bmp", nil, formatRasterImage},
	{"image/x-ms-bmp", ".bmp", nil, formatRasterImage},

	// Other image formats retain their filename extension and may use the
	// generic image barcode path, but are not raster-to-PDF inputs.
	{"image/avif", ".avif", nil, 0},
	{"image/svg+xml", ".svg", nil, 0},
	{ImageDjVu, ".djvu", []string{".djv", ".djvu"}, formatDjVu},
	{"image/x-djvu", ".djvu", nil, formatDjVu},
	{"image/djvu", ".djvu", nil, formatDjVu},

	// Email and Outlook containers.
	{MessageRFC822, ".eml", []string{".eml"}, formatEmail},
	{"message/global", ".eml", nil, formatEmail},
	{ApplicationMSOutlook, ".msg", []string{".msg"}, formatOutlook},
	{"application/x-outlook-msg", ".msg", nil, formatOutlook},
	{"application/ms-outlook", ".msg", nil, formatOutlook},

	// Text formats whose source bytes are already extracted content.
	{TextPlain, ".txt", nil, formatDirectText},
	{TextMarkdown, ".md", []string{".md", ".markdown"}, formatDirectText},
	{TextHTML, ".html", nil, formatDirectText},
}

var (
	formatsByMIME           map[string]Format
	mimeByFilenameExtension map[string]string
)

func init() {
	formatsByMIME = make(map[string]Format, len(formatDefinitions))
	mimeByFilenameExtension = make(map[string]string)
	for _, definition := range formatDefinitions {
		if definition.mime != normalizeMIME(definition.mime) {
			panic("mimeutil: format MIME must be normalized: " + definition.mime)
		}
		if _, exists := formatsByMIME[definition.mime]; exists {
			panic("mimeutil: duplicate format MIME: " + definition.mime)
		}
		formatsByMIME[definition.mime] = Format{
			MIME: definition.mime, Extension: definition.extension, flags: definition.flags,
		}
		for _, extension := range definition.filenameExtensions {
			if previous := mimeByFilenameExtension[extension]; previous != "" {
				panic("mimeutil: duplicate filename extension: " + extension)
			}
			mimeByFilenameExtension[extension] = definition.mime
		}
	}
}

// Lookup normalizes a MIME label, ignoring parameters such as charset, and
// returns its filename extension and processing capabilities.
func Lookup(mimeType string) Format {
	mimeType = normalizeMIME(mimeType)
	if format, ok := formatsByMIME[mimeType]; ok {
		return format
	}
	return Format{MIME: mimeType}
}

// Extension returns the conventional filename extension for a MIME type.
// Unknown types return "" so callers leave the name without an extension.
func Extension(mimeType string) string {
	return Lookup(mimeType).Extension
}

func normalizeMIME(mimeType string) string {
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	return strings.ToLower(strings.TrimSpace(mimeType))
}
