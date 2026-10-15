// SPDX-License-Identifier: AGPL-3.0-or-later

package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/imgpdf"
	"github.com/johnnybravo-xyz/suchi/core/sandbox"
)

const PrerequisitesReportVersion = 1

type ToolStatus struct {
	Name         string `json:"name"`
	ResolvedName string `json:"resolved_name,omitempty"`
	Path         string `json:"path,omitempty"`
	Available    bool   `json:"available"`
	Capability   string `json:"capability"`
	Guidance     string `json:"guidance,omitempty"`
}

type CapabilityStatus struct {
	Available bool     `json:"available"`
	Languages []string `json:"languages,omitempty"`
	Detail    string   `json:"detail"`
	Guidance  string   `json:"guidance,omitempty"`
}

type PrerequisitesReport struct {
	Version              int              `json:"version"`
	Tools                []ToolStatus     `json:"tools"`
	EnglishTesseractData CapabilityStatus `json:"english_tesseract_data"`
	ImageToPDFConversion CapabilityStatus `json:"image_to_pdf_conversion"`
	Degraded             bool             `json:"degraded"`
}

type toolSpec struct {
	name       string
	fallbacks  []string
	capability string
	guidance   string
}

var prerequisiteTools = []toolSpec{
	{name: "qpdf", capability: "PDF normalization and encrypted-PDF detection", guidance: "Install qpdf (package qpdf)."},
	{name: "pdftotext", capability: "Text extraction from text-native PDFs", guidance: "Install Poppler tools (package poppler or poppler-utils)."},
	{name: "pdftoppm", capability: "PDF rasterization for OCR and QR-split detection", guidance: "Install Poppler tools (package poppler or poppler-utils)."},
	{name: "tesseract", capability: "OCR for scanned documents", guidance: "Install Tesseract OCR and its English language data."},
	{name: "ocrmypdf", capability: "Searchable-PDF OCR output", guidance: "Install OCRmyPDF."},
	{name: "djvutxt", capability: "DjVu text extraction", guidance: "Install DjVuLibre tools."},
	{name: "anydoc", capability: "Office-document text extraction", guidance: "Install anydoc from the Suchi release matching this operating system and architecture."},
	{name: "magick", fallbacks: []string{"convert"}, capability: "Image-to-PDF conversion before OCR", guidance: "Install ImageMagick with PNG input and PDF output support."},
	{name: "msgconvert", capability: "Outlook .msg conversion", guidance: "Install the Email::Outlook::Message msgconvert tool."},
	{name: "mbsync", capability: "Mailbox synchronization", guidance: "Install isync/mbsync before enabling mailbox ingestion."},
}

func InspectTools(lookPath func(string) (string, error)) []ToolStatus {
	tools := make([]ToolStatus, 0, len(prerequisiteTools))
	for _, spec := range prerequisiteTools {
		status := ToolStatus{Name: spec.name, Capability: spec.capability}
		path, err := lookPath(spec.name)
		if err == nil {
			status.Available = true
			status.ResolvedName = spec.name
			status.Path = path
		} else {
			for _, fallback := range spec.fallbacks {
				path, err = lookPath(fallback)
				if err == nil {
					status.Available = true
					status.ResolvedName = fallback
					status.Path = path
					break
				}
			}
		}
		if !status.Available {
			status.Guidance = spec.guidance
		}
		tools = append(tools, status)
	}
	return tools
}

func CheckPrerequisites(ctx context.Context) PrerequisitesReport {
	tools := InspectTools(exec.LookPath)
	report := PrerequisitesReport{
		Version: PrerequisitesReportVersion,
		Tools:   tools,
	}
	var tesseractPath, imageMagickPath string
	for _, tool := range tools {
		if !tool.Available {
			report.Degraded = true
		}
		switch tool.Name {
		case "tesseract":
			tesseractPath = tool.Path
		case "magick":
			imageMagickPath = tool.Path
		}
	}
	report.EnglishTesseractData = checkTesseractData(ctx, tesseractPath)
	report.ImageToPDFConversion = checkImageToPDF(ctx, imageMagickPath)
	if !report.EnglishTesseractData.Available || !report.ImageToPDFConversion.Available {
		report.Degraded = true
	}
	return report
}

func checkTesseractData(ctx context.Context, tesseractPath string) CapabilityStatus {
	const guidance = "Install the Tesseract English language data pack (eng.traineddata)."
	if tesseractPath == "" {
		return CapabilityStatus{Detail: "tesseract is not available", Guidance: guidance}
	}
	result, err := sandbox.Run(ctx, sandbox.Opts{
		Args:      []string{tesseractPath, "--list-langs"},
		Timeout:   5 * time.Second,
		MaxStdout: 1 << 20,
		MaxStderr: 64 << 10,
	})
	if err != nil {
		detail := err.Error()
		if result != nil && strings.TrimSpace(string(result.Stderr)) != "" {
			detail += ": " + strings.TrimSpace(string(result.Stderr))
		}
		return CapabilityStatus{Detail: detail, Guidance: guidance}
	}
	if result.StdoutTruncated {
		return CapabilityStatus{Detail: "tesseract language list exceeded 1 MiB", Guidance: guidance}
	}
	languages, err := classifyOCRLanguages(result.Stdout)
	if err != nil {
		return CapabilityStatus{Detail: err.Error(), Guidance: guidance}
	}
	for _, language := range languages {
		if language == "eng" {
			return CapabilityStatus{Available: true, Languages: languages, Detail: "English OCR data is available"}
		}
	}
	return CapabilityStatus{Languages: languages, Detail: "English OCR data is missing", Guidance: guidance}
}

func classifyOCRLanguages(output []byte) ([]string, error) {
	installed := make(map[string]struct{})
	for _, line := range strings.Split(string(output), "\n") {
		language := strings.TrimSpace(line)
		if language == "" || strings.ContainsAny(language, " \t") {
			continue
		}
		installed[language] = struct{}{}
	}
	if len(installed) == 0 {
		return nil, errors.New("tesseract returned no installed languages")
	}
	languages := make([]string, 0, len(installed))
	for language := range installed {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	return languages, nil
}

func checkImageToPDF(ctx context.Context, binary string) CapabilityStatus {
	const guidance = "Install ImageMagick with PNG input and PDF output enabled; images remain archived without this conversion."
	if binary == "" {
		return CapabilityStatus{Detail: "ImageMagick is not available", Guidance: guidance}
	}
	imageBytes, err := prerequisitePNG()
	if err != nil {
		return CapabilityStatus{Detail: err.Error(), Guidance: guidance}
	}
	result, err := imgpdf.Convert(ctx, bytes.NewReader(imageBytes), slog.New(slog.NewTextHandler(io.Discard, nil)), imgpdf.Options{
		Binary:         binary,
		Timeout:        15 * time.Second,
		MaxOutputBytes: 1 << 20,
		Ext:            "png",
	})
	if err != nil {
		return CapabilityStatus{Detail: err.Error(), Guidance: guidance}
	}
	if result.Skipped {
		detail := strings.TrimSpace(result.StderrTail)
		if detail == "" {
			detail = "ImageMagick did not produce a PDF"
		}
		return CapabilityStatus{Detail: detail, Guidance: guidance}
	}
	if !bytes.HasPrefix(result.PDF, []byte("%PDF-")) {
		return CapabilityStatus{Detail: "ImageMagick returned invalid PDF data", Guidance: guidance}
	}
	return CapabilityStatus{Available: true, Detail: "temporary PNG converted to PDF"}
}

func prerequisitePNG() ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.White)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		return nil, fmt.Errorf("encode prerequisite PNG: %w", err)
	}
	return output.Bytes(), nil
}
