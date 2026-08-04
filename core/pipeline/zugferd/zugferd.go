// Package zugferd extracts structured invoice data from ZUGFeRD /
// Factur-X / XRechnung PDF/A-3 files.
//
// ZUGFeRD is a PDF/A-3 that carries a Cross-Industry Invoice (CII) XML
// as an embedded file — the XML is the machine-readable ground truth,
// the visual PDF is for humans. Extracting the XML lets us auto-populate
// custom fields (invoice number, total, currency, issue date, seller)
// that would otherwise depend on OCR + rules or an LLM.
//
// The extractor is optional and best-effort: PDFs without an embedded
// invoice XML return (nil, nil). Failure to parse XML returns (nil,
// err) so the caller can log at Warn and continue — the doc is already
// ingested, structured metadata is a bonus.
//
// Attachment discovery uses `qpdf --list-attachments` then
// `qpdf --show-attachment=<name>` because qpdf is the only PDF binary
// we already require in the "full" image. Format detection is by name
// (case-insensitive): factur-x.xml, zugferd-invoice.xml, xrechnung.xml.
package zugferd

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/suchi-dms/suchi/core/sandbox"
)

// Defaults.
const (
	DefaultBinary   = "qpdf"
	DefaultTimeout  = 15 * time.Second
	DefaultMaxBytes = 4 * 1024 * 1024 // 4 MiB — invoice XMLs are tiny in practice
)

// Options carries per-call knobs.
type Options struct {
	Binary   string
	Timeout  time.Duration
	MaxBytes int64
}

// Invoice is the extracted, normalized projection. Fields left empty
// when the source XML didn't carry them.
type Invoice struct {
	Number     string  // BT-1
	IssueDate  string  // BT-2 (YYYY-MM-DD)
	Currency   string  // BT-5 (ISO 4217)
	TotalGross float64 // BT-112 (Grand total amount)
	TotalNet   float64 // BT-109 (Sum of line net amounts)
	SellerName string  // BT-27
	SellerVAT  string  // BT-31
	BuyerName  string  // BT-44
	// SourceFile is the attachment name we pulled the XML from — kept
	// for debugging + audit.
	SourceFile string
}

// Extract discovers a ZUGFeRD-family attachment in pdfPath (on disk) and
// returns the parsed invoice. Returns (nil, nil) when the PDF has no
// recognised invoice attachment — that's the common case (most PDFs
// aren't ZUGFeRD) and it must not be an error.
func Extract(ctx context.Context, pdfPath string, log *slog.Logger, opts Options) (*Invoice, error) {
	log = log.With("component", "zugferd")

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		log.Debug("zugferd.skip.no_binary", "binary", binary)
		return nil, nil
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	maxBytes := opts.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}

	// 1) list attachments.
	listRes, err := sandbox.Run(ctx, sandbox.Opts{
		Args:      []string{binary, "--list-attachments", pdfPath},
		Timeout:   timeout,
		MaxStdout: 64 * 1024,
	})
	if err != nil {
		// qpdf exits 3 when there are no attachments — treat any error
		// here as "no ZUGFeRD" and move on.
		log.Debug("zugferd.list_attachments.skip", "exit", listRes.ExitCode)
		return nil, nil
	}
	name := pickInvoiceAttachment(string(listRes.Stdout))
	if name == "" {
		return nil, nil
	}

	// 2) show that attachment. Route stdout to a bounded buffer.
	showRes, err := sandbox.Run(ctx, sandbox.Opts{
		Args:      []string{binary, "--show-attachment=" + name, pdfPath},
		Timeout:   timeout,
		MaxStdout: maxBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("zugferd: show-attachment %s: %w", name, err)
	}
	if showRes.StdoutTruncated {
		return nil, fmt.Errorf("zugferd: attachment %s exceeded %d bytes", name, maxBytes)
	}

	inv, err := parseCII(showRes.Stdout)
	if err != nil {
		return nil, fmt.Errorf("zugferd: parse %s: %w", name, err)
	}
	inv.SourceFile = name
	log.Info("zugferd.extracted",
		"file", name,
		"number", inv.Number,
		"total", inv.TotalGross,
		"currency", inv.Currency)
	return inv, nil
}

// ExtractBytes is a convenience wrapper that writes the PDF to a
// tempfile and calls Extract. Use when the caller already has the
// bytes in memory (post-qpdf normalized output).
func ExtractBytes(ctx context.Context, pdf []byte, log *slog.Logger, opts Options) (*Invoice, error) {
	dir, err := os.MkdirTemp("", "suchi-zugferd-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(p, pdf, 0o600); err != nil {
		return nil, err
	}
	return Extract(ctx, p, log, opts)
}

// pickInvoiceAttachment scans qpdf's --list-attachments output for a
// filename in the ZUGFeRD / Factur-X / XRechnung name set. Case-
// insensitive; returns the first match verbatim so --show-attachment
// gets the exact key qpdf indexed on.
//
// qpdf output looks like:
//
//	factur-x.xml -> 42
//	extra.txt -> 43
func pickInvoiceAttachment(list string) string {
	for _, line := range strings.Split(list, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Trim trailing " -> <objnum>" if present.
		if i := strings.Index(line, " -> "); i > 0 {
			line = line[:i]
		}
		low := strings.ToLower(line)
		if low == "factur-x.xml" || low == "zugferd-invoice.xml" || low == "xrechnung.xml" {
			return line
		}
	}
	return ""
}

// ---------- CII XML → Invoice ----------
//
// The Cross-Industry Invoice schema is deeply nested; we extract only
// the fields we need with a purpose-built struct instead of pulling in
// a full CII binding. Order-independent field wiring uses
// namespace-agnostic matching via `xml:",any"` where necessary. The
// happy path is a full CII document; malformed docs return (nil, err).

func parseCII(data []byte) (*Invoice, error) {
	trimmed := trimBOM(data)
	if len(trimmed) < 32 {
		return nil, errors.New("empty or too-short XML")
	}
	var doc ciiDoc
	if err := xml.Unmarshal(trimmed, &doc); err != nil {
		return nil, err
	}
	inv := &Invoice{
		Number:     strings.TrimSpace(doc.Doc.ID),
		IssueDate:  parseCIIDate(doc.Doc.IssueDateTime.String),
		Currency:   strings.TrimSpace(doc.Trade.Settlement.Currency),
		TotalGross: parseFloat(doc.Trade.Settlement.Sum.Grand),
		TotalNet:   parseFloat(doc.Trade.Settlement.Sum.LineTotal),
		SellerName: strings.TrimSpace(doc.Trade.Agreement.Seller.Name),
		SellerVAT:  pickSellerVAT(doc.Trade.Agreement.Seller.TaxRegs),
		BuyerName:  strings.TrimSpace(doc.Trade.Agreement.Buyer.Name),
	}
	return inv, nil
}

// ciiDoc mirrors the tiny sliver of CrossIndustryInvoice we need. All
// element names are matched by localname only — the CII namespaces
// (rsm/ram/udt) change between profiles; the local names are stable
// across BASIC-WL / BASIC / EN 16931 / EXTENDED.
type ciiDoc struct {
	XMLName xml.Name       `xml:"CrossIndustryInvoice"`
	Doc     ciiExchDoc     `xml:"ExchangedDocument"`
	Trade   ciiTransaction `xml:"SupplyChainTradeTransaction"`
}

type ciiExchDoc struct {
	ID            string  `xml:"ID"`
	IssueDateTime ciiDate `xml:"IssueDateTime"`
}

type ciiDate struct {
	String string `xml:"DateTimeString"`
}

type ciiTransaction struct {
	Agreement  ciiAgreement  `xml:"ApplicableHeaderTradeAgreement"`
	Settlement ciiSettlement `xml:"ApplicableHeaderTradeSettlement"`
}

type ciiAgreement struct {
	Seller ciiParty `xml:"SellerTradeParty"`
	Buyer  ciiParty `xml:"BuyerTradeParty"`
}

type ciiParty struct {
	Name    string      `xml:"Name"`
	TaxRegs []ciiTaxReg `xml:"SpecifiedTaxRegistration"`
}

type ciiTaxReg struct {
	ID ciiSchemeID `xml:"ID"`
}

type ciiSchemeID struct {
	Value  string `xml:",chardata"`
	Scheme string `xml:"schemeID,attr"`
}

type ciiSettlement struct {
	Currency string     `xml:"InvoiceCurrencyCode"`
	Sum      ciiSummary `xml:"SpecifiedTradeSettlementHeaderMonetarySummation"`
}

type ciiSummary struct {
	LineTotal string `xml:"LineTotalAmount"`
	Grand     string `xml:"GrandTotalAmount"`
}

// pickSellerVAT returns the schemeID="VA" registration if present,
// falls back to the first entry. Empty when the party has no
// tax registration at all.
func pickSellerVAT(regs []ciiTaxReg) string {
	if len(regs) == 0 {
		return ""
	}
	for _, r := range regs {
		if strings.EqualFold(r.ID.Scheme, "VA") && r.ID.Value != "" {
			return strings.TrimSpace(r.ID.Value)
		}
	}
	return strings.TrimSpace(regs[0].ID.Value)
}

// parseCIIDate turns CII's "102" format (yyyymmdd) into ISO 8601
// yyyy-mm-dd. Returns "" for empty or malformed inputs.
func parseCIIDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) != 8 {
		return ""
	}
	// Cheap manual formatter — no time.Parse dance for a fixed layout.
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return s[:4] + "-" + s[4:6] + "-" + s[6:]
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0
	}
	return f
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}
