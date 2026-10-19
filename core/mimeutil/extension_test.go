// SPDX-License-Identifier: AGPL-3.0-or-later

package mimeutil

import "testing"

func TestLookup(t *testing.T) {
	tests := []struct {
		mime       string
		extension  string
		pdf        bool
		anyDoc     bool
		raster     bool
		image      bool
		email      bool
		outlook    bool
		djvu       bool
		directText bool
	}{
		{mime: "application/pdf", extension: ".pdf", pdf: true},
		{mime: "application/vnd.ms-word.document.macroenabled.12", extension: ".docm", anyDoc: true},
		{mime: "text/rtf; charset=utf-8", extension: ".rtf", anyDoc: true},
		{mime: " IMAGE/X-MS-BMP ; charset=binary", extension: ".bmp", raster: true, image: true},
		{mime: "image/x-djvu", extension: ".djvu", image: true, djvu: true},
		{mime: "image/svg+xml", extension: ".svg", image: true},
		{mime: "image/x-custom", image: true},
		{mime: "message/global", extension: ".eml", email: true},
		{mime: "application/x-outlook-msg", extension: ".msg", outlook: true},
		{mime: "text/markdown", extension: ".md", directText: true},
		{mime: "text/csv", extension: ".csv", anyDoc: true},
		{mime: "application/octet-stream"},
		{mime: ""},
	}
	for _, tt := range tests {
		got := Lookup(tt.mime)
		if got.Extension != tt.extension ||
			got.IsPDF() != tt.pdf ||
			got.IsAnyDoc() != tt.anyDoc ||
			got.IsRasterImage() != tt.raster ||
			got.IsImage() != tt.image ||
			got.IsEmail() != tt.email ||
			got.IsOutlook() != tt.outlook ||
			got.IsDjVu() != tt.djvu ||
			got.IsDirectText() != tt.directText {
			t.Errorf("Lookup(%q) = %+v, capabilities pdf=%v anydoc=%v raster=%v image=%v email=%v outlook=%v djvu=%v text=%v",
				tt.mime, got, got.IsPDF(), got.IsAnyDoc(), got.IsRasterImage(),
				got.IsImage(), got.IsEmail(), got.IsOutlook(), got.IsDjVu(), got.IsDirectText())
		}
	}
}

func TestExtensionAliases(t *testing.T) {
	cases := map[string]string{
		"application/epub":       ".epub",
		"application/ms-outlook": ".msg",
		"application/vnd.ms-excel.sheet.binary.macroenabled.12":                  ".xlsb",
		"application/vnd.ms-powerpoint.slideshow.macroenabled.12":                ".ppsm",
		"application/vnd.openxmlformats-officedocument.presentationml.slideshow": ".ppsx",
		"image/heif-sequence":       ".heif",
		"image/pjpeg":               ".jpg",
		"image/x-tiff":              ".tiff",
		"message/rfc822":            ".eml",
		"text/plain; charset=utf-8": ".txt",
		"application/octet-stream":  "",
	}
	for mimeType, want := range cases {
		if got := Extension(mimeType); got != want {
			t.Errorf("Extension(%q) = %q, want %q", mimeType, got, want)
		}
	}
}
